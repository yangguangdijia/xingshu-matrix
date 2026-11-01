package observability

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	maxDraftBody    = 64 << 10
	maxDraftTimeout = 30 * time.Second
)

type datasourceDraftInput struct {
	dsInput
	DatasourceID flexibleClusterID `json:"datasource_id"`
	Timeout      *int              `json:"timeout_sec"`
}

// validate rejects ambiguous ownership and transport inputs before any lookup.
// Unknown options remain available for adapter metadata but must be bounded scalars.
func (in *datasourceDraftInput) validate(clusterID string) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 || hasDraftControl(in.Name) {
		return httpx.Validation("name is required and must be at most 64 characters")
	}
	if in.Timeout != nil && (*in.Timeout < 1 || *in.Timeout > 300) {
		return httpx.Validation("timeout_sec must be 1..300")
	}
	in.TimeoutSec = 30
	if in.Timeout != nil {
		in.TimeoutSec = *in.Timeout
	}
	rawURL := strings.TrimSpace(in.URL)
	if strings.Contains(rawURL, "#") {
		return httpx.Validation("url must not contain a fragment")
	}
	if err := in.dsInput.validate(); err != nil {
		return err
	}
	u, err := url.Parse(in.URL)
	if err != nil || len(in.URL) > 2048 || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(in.URL, "#") ||
		strings.ContainsAny(in.URL, "\\ \t\r\n") {
		return httpx.Validation("url must be an absolute http(s) URL without credentials, query or fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return httpx.Validation("url port must be 1..65535")
		}
	}
	if len(in.Username) > 256 || hasDraftControl(in.Username) || len(in.Env) > 128 || hasDraftControl(in.Env) {
		return httpx.Validation("invalid username or environment")
	}
	if in.Secret != nil && (len(*in.Secret) > 8192 || hasDraftControl(*in.Secret)) {
		return httpx.Validation("invalid authentication credential")
	}
	if in.AuthType == "basic" && (strings.TrimSpace(in.Username) == "" || strings.Contains(in.Username, ":")) {
		return httpx.Validation("basic authentication requires a valid username")
	}
	if len(in.Options) > 32 {
		return httpx.Validation("too many datasource options")
	}
	for key, value := range in.Options {
		if key == "" || len(key) > 128 || hasDraftControl(key) {
			return httpx.Validation("invalid datasource option name")
		}
		switch v := value.(type) {
		case string:
			if len(v) > 8192 || hasDraftControl(v) {
				return httpx.Validation("invalid datasource option value")
			}
		case json.Number, bool, nil:
		default:
			return httpx.Validation("datasource options must contain scalar values")
		}
	}
	for _, key := range []string{"scope", "cluster_name", "org_id", "index", "time_field", "message_field", "log_selector"} {
		if value, exists := in.Options[key]; exists {
			if _, ok := value.(string); !ok {
				return httpx.Validation("invalid datasource option type")
			}
		}
	}
	if scopeKind(in.Options) != "cluster" {
		return httpx.Validation("draft test requires an explicitly owned cluster datasource")
	}
	for _, key := range []string{"cluster_label", "cluster_values"} {
		if _, exists := in.Options[key]; exists {
			return httpx.Validation("cluster draft cannot declare central ownership options")
		}
	}
	if value, exists := in.Options["cluster_id"]; exists {
		var text string
		switch v := value.(type) {
		case string:
			text = strings.TrimSpace(v)
		case json.Number:
			text = v.String()
		default:
			return httpx.Validation("invalid options.cluster_id")
		}
		id, err := strconv.ParseInt(text, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != clusterID {
			return httpx.Validation("query and datasource cluster_id disagree")
		}
		// Use the same canonical owner representation as persistent writes.
		in.Options["cluster_id"] = clusterID
	}
	if index := optionString(in.Options, "index"); strings.ContainsAny(index, "/?#") {
		return httpx.Validation("invalid Elasticsearch index")
	}
	return nil
}

func hasDraftControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

// draftSecret never moves a stored credential to a different endpoint or identity.
func draftSecret(in *dsInput, current *DataSource) (string, error) {
	if in.AuthType == "none" {
		return "", nil
	}
	if in.Secret != nil && *in.Secret != "" {
		return *in.Secret, nil
	}
	if current == nil || current.secretPlain == "" || current.Type != in.Type ||
		current.AuthType != in.AuthType || current.Username != in.Username ||
		strings.TrimRight(current.URL, "/") != in.URL {
		return "", httpx.Validation("authentication credential required; blank credentials can only preserve the same endpoint and authentication identity")
	}
	return current.secretPlain, nil
}

func (m *Module) testDraftDS(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	if !p.Can(PermDSManage) {
		httpx.Fail(c, httpx.Forbidden("datasource:manage required"))
		return
	}
	requested := c.Request.URL.Query()["cluster_id"]
	if len(requested) != 1 {
		httpx.Fail(c, httpx.Validation("one explicit cluster_id is required"))
		return
	}
	id, err := strconv.ParseInt(requested[0], 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, httpx.Validation("cluster_id must be a positive integer"))
		return
	}
	clusterID := strconv.FormatInt(id, 10)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDraftBody)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var in datasourceDraftInput
	err = decoder.Decode(&in)
	var extra any
	if err == nil {
		err = decoder.Decode(&extra)
		if errors.Is(err, io.EOF) {
			err = nil
		} else if err == nil {
			err = errors.New("multiple JSON values")
		}
	}
	if err != nil {
		// Decoder errors can contain credentials or other user-controlled values.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.Fail(c, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeValidation, "draft body exceeds 64 KiB"))
		} else {
			httpx.Fail(c, httpx.Validation("invalid datasource draft body"))
		}
		return
	}
	if err := in.validate(clusterID); err != nil {
		httpx.Fail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), maxDraftTimeout)
	defer cancel()
	if err := m.validateDSScope(ctx, p, &in.dsInput); err != nil {
		httpx.Fail(c, err)
		return
	}
	// This also checks query/body ownership when the body uses cluster_name only.
	draft := &DataSource{Type: in.Type, URL: in.URL, Env: in.Env, AuthType: in.AuthType,
		Username: in.Username, TLSSkipVerify: in.TLSSkipVerify, TimeoutSec: in.TimeoutSec, Options: in.Options}
	if _, err := m.resolveSourceScope(ctx, p, draft, clusterID); err != nil {
		httpx.Fail(c, err)
		return
	}
	var current *DataSource
	if in.DatasourceID != "" {
		dsID, _ := strconv.ParseInt(string(in.DatasourceID), 10, 64)
		current, err = m.loadDS(ctx, p, dsID)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		scope, err := m.resolveSourceScope(ctx, p, current, clusterID)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if scope.Central || current.Type != in.Type {
			httpx.Fail(c, httpx.Validation("datasource_id must identify this cluster's own datasource of the same type"))
			return
		}
	}
	draft.secretPlain, err = draftSecret(&in.dsInput, current)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	start := time.Now()
	testCtx, testCancel := context.WithTimeout(ctx, time.Duration(draft.TimeoutSec)*time.Second)
	defer testCancel()
	upstream := newClient(draft)
	defer upstream.http.CloseIdleConnections()
	// A health check cannot redirect credentials to a different service.
	upstream.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, err = upstream.Test(testCtx)
	result := gin.H{"ok": err == nil, "latency_ms": time.Since(start).Milliseconds()}
	if err != nil {
		result["error"] = "datasource health check failed; verify endpoint, authentication and service availability"
		if testCtx.Err() != nil {
			result["error"] = "datasource health check timed out or was canceled"
		}
	}
	httpx.OK(c, result)
}
