package cicd

// This file adapts Argo CD as an external deployment backend for the cicd
// module. Connections (URL + bearer token + environment + project) are stored
// in cicd_argocd; tokens are encrypted with Deps.Crypto and masked on output.
//
// The API is mounted by the main module through registerArgoRoutes, which also
// registers the approval type "cicd.argocd.sync". A sync is always pinned to a
// concrete revision recorded in the approval payload; it is executed with
// prune=false and force=false. Every operation is scoped to the connection's
// environment with strict RBAC checks.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	argoApprovalType = "cicd.argocd.sync"
	argoTokenMask    = "********"

	argoMaxBody     int64 = 4 << 20 // 4 MiB
	argoMaxToken          = 8192
	argoMaxRevision       = 256

	argoRequestTimeout = 20 * time.Second
)

var (
	argoEnvRe          = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}_.-]{0,63}$`)
	argoNameRe         = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,251}[A-Za-z0-9])?$`)
	argoCommitRe       = regexp.MustCompile(`^(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$`)
	argoChartVersionRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
)

// argoConfig is one Argo CD connection.
type argoConfig struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Token     string    `json:"token,omitempty"` // input only; output is masked
	Env       string    `json:"env"`
	Project   string    `json:"project"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	tokenEnc string
}

func (c *argoConfig) public() *argoConfig {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Token = ""
	if c.tokenEnc != "" {
		cp.Token = argoTokenMask
	}
	return &cp
}

type argoConfigIn struct {
	Name    string `json:"name" binding:"required,max=64"`
	URL     string `json:"url" binding:"required,max=2048"`
	Token   string `json:"token"`
	Env     string `json:"env" binding:"required,max=64"`
	Project string `json:"project" binding:"max=253"`
}

type argoSyncIn struct {
	Revision string `json:"revision"`
	Reason   string `json:"reason"`
}

// argoSyncPayload is deliberately minimal: it identifies the connection, the
// application and the revision to sync. It never contains a token.
type argoSyncPayload struct {
	ConfigID      int64      `json:"config_id"`
	App           string     `json:"app"`
	Revision      string     `json:"revision"`
	SpecHash      string     `json:"spec_hash,omitempty"`
	ConfigVersion *time.Time `json:"config_version,omitempty"`
}

const argoCols = `id,name,url,token_enc,env,project,created_at,updated_at`

func scanArgoConfig(row pgx.Row) (*argoConfig, error) {
	var c argoConfig
	if err := row.Scan(&c.ID, &c.Name, &c.URL, &c.tokenEnc, &c.Env, &c.Project, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (m *Module) loadArgoConfig(ctx context.Context, id int64) (*argoConfig, error) {
	return scanArgoConfig(m.db.QueryRow(ctx, `SELECT `+argoCols+` FROM cicd_argocd WHERE id=$1`, id))
}

// ---------------------------------------------------------------- validation

func validArgoEnv(env string) bool { return argoEnvRe.MatchString(env) }

func validArgoApp(name string) bool {
	return name != "" && len(name) <= 253 && !strings.Contains(name, "..") && argoNameRe.MatchString(name)
}

func validArgoProject(p string) bool {
	return p != "" && len(p) <= 253 && !strings.Contains(p, "..") && argoNameRe.MatchString(p)
}

func validArgoRevision(rev string) bool {
	if rev == "" || len(rev) > argoMaxRevision || strings.TrimSpace(rev) != rev {
		return false
	}
	for _, r := range rev {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validArgoDisplay(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// normalizeArgoURL validates a connection URL and removes a trailing slash.
// Only http(s), no userinfo, query or fragment are accepted; TLS verification
// is never disabled by this module.
func normalizeArgoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || inlineSecretText(raw) {
		return "", httpx.Validation("argocd url is required (max 2048)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return "", httpx.Validation("invalid argocd url")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", httpx.Validation("argocd url must use http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", httpx.Validation("argocd url must include a host")
	}
	if u.User != nil {
		return "", httpx.Validation("argocd url must not contain credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", httpx.Validation("argocd url must not contain a query or fragment")
	}
	if strings.ContainsAny(u.Host, " \t\r\n") {
		return "", httpx.Validation("invalid argocd url")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return "", httpx.Validation("argocd url must not contain dot segments")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func validateArgoConfigInput(in *argoConfigIn, creating bool) (string, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !validArgoDisplay(in.Name, 64) || inlineSecretText(in.Name) {
		return "", httpx.Validation("argocd name is required (max 64)")
	}
	in.Env = strings.TrimSpace(in.Env)
	if !validArgoEnv(in.Env) {
		return "", httpx.Validation("env must use up to 64 letters, digits, dots, hyphens or underscores")
	}
	in.Project = strings.TrimSpace(in.Project)
	if in.Project == "" {
		in.Project = "default"
	}
	if !validArgoProject(in.Project) || inlineSecretText(in.Project) {
		return "", httpx.Validation("invalid argocd project")
	}
	u, err := normalizeArgoURL(in.URL)
	if err != nil {
		return "", err
	}
	in.URL = u
	in.Token = strings.TrimSpace(in.Token)
	if creating && in.Token == argoTokenMask {
		return "", httpx.Validation("argocd token mask is only valid when updating a config")
	}
	if len(in.Token) > argoMaxToken {
		return "", httpx.Validation("argocd token is too long")
	}
	for _, r := range in.Token {
		if r < 0x20 || r == 0x7f {
			return "", httpx.Validation("invalid argocd token")
		}
	}
	return u, nil
}

func requireArgoScope(c *gin.Context, env string) bool {
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return false
	}
	if env == "" {
		httpx.Fail(c, httpx.Forbidden("argocd config has no environment"))
		return false
	}
	if !p.CanEnv(env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+env))
		return false
	}
	return true
}

func argoScopeAll(scope []string) bool {
	if scope == nil {
		return true
	}
	for _, e := range scope {
		if e == "*" {
			return true
		}
	}
	return false
}

func (m *Module) argoConfigFromPath(c *gin.Context) (*argoConfig, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, false
	}
	cfg, err := m.loadArgoConfig(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	if !requireArgoScope(c, cfg.Env) {
		return nil, false
	}
	return cfg, true
}

// ---------------------------------------------------------------- crypto

func (m *Module) encryptArgoToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	if m == nil || m.d == nil || m.d.Crypto == nil {
		return "", errors.New("cicd: crypto is not configured")
	}
	return m.d.Crypto.Encrypt(token)
}

func (m *Module) argoToken(cfg *argoConfig) (string, error) {
	if cfg == nil || cfg.tokenEnc == "" {
		return "", nil
	}
	if m == nil || m.d == nil || m.d.Crypto == nil {
		return "", errors.New("cicd: crypto is not configured")
	}
	tok, err := m.d.Crypto.Decrypt(cfg.tokenEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt argocd token: %w", err)
	}
	return tok, nil
}

// ---------------------------------------------------------------- HTTP client

// argoHTTPClient clones the module client and installs conservative redirect
// handling so a token is never forwarded to another host or downgraded to http.
// A nil Transport keeps net/http's default TLS verification enabled.
func (m *Module) argoHTTPClient() *http.Client {
	var base *http.Client
	if m != nil {
		base = m.http
	}
	if base == nil {
		base = &http.Client{}
	}
	hc := *base
	if hc.Timeout <= 0 || hc.Timeout > argoRequestTimeout {
		hc.Timeout = argoRequestTimeout
	}
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("argocd: too many redirects")
		}
		if len(via) > 0 {
			first := via[0].URL
			if !strings.EqualFold(req.URL.Host, first.Host) {
				return errors.New("argocd: cross-host redirect rejected")
			}
			if strings.EqualFold(first.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
				return errors.New("argocd: insecure redirect rejected")
			}
		}
		return nil
	}
	return &hc
}

// argoEndpoint joins the connection base URL (which may include a path prefix)
// with an API path. Path segments and query values are encoded by net/url.
func argoEndpoint(base, apiPath string, q url.Values) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.Opaque != "" {
		return "", errors.New("invalid argocd base url")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("invalid argocd base url")
	}
	if u.User != nil {
		return "", errors.New("argocd base url must not contain credentials")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(apiPath, "/")
	u.RawPath = ""
	u.RawQuery = ""
	u.ForceQuery = false
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), nil
}

var errArgoBodyTooLarge = errors.New("argocd response exceeds size limit")

func readArgoBody(r io.Reader, max int64) ([]byte, error) {
	if max < 0 {
		max = argoMaxBody
	}
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errArgoBodyTooLarge
	}
	return b, nil
}

type argoStatusError struct {
	Method string
	Path   string
	Status int
}

func (e *argoStatusError) Error() string {
	return fmt.Sprintf("argocd %s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// argoHTTPError converts an external error into a client-safe error. In
// particular it never copies a response body or request headers, so a token
// cannot leak through an upstream error.
func argoHTTPError(err error) error {
	if err == nil {
		return nil
	}
	var he *httpx.Error
	if errors.As(err, &he) {
		return he
	}
	if errors.Is(err, errArgoBodyTooLarge) {
		return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "Argo CD 响应超过大小限制").Wrap(err)
	}
	var se *argoStatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusNotFound:
			return httpx.NotFound("Argo CD 资源不存在").Wrap(err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "Argo CD 凭据无效或权限不足").Wrap(err)
		case http.StatusBadRequest:
			return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "Argo CD 拒绝了请求").Wrap(err)
		default:
			return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream,
				fmt.Sprintf("Argo CD 返回 HTTP %d", se.Status)).Wrap(err)
		}
	}
	return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "无法连接 Argo CD").Wrap(err)
}

// argoDo performs one Argo CD REST call with the request context, a hard
// timeout, default TLS verification and a bounded response body.
func (m *Module) argoDo(ctx context.Context, cfg *argoConfig, method, apiPath string, q url.Values, body any) ([]byte, error) {
	if m == nil {
		return nil, httpx.Internal(errors.New("argocd module is not configured"))
	}
	if cfg == nil {
		return nil, httpx.Internal(errors.New("argocd config is missing"))
	}
	token, err := m.argoToken(cfg)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	endpoint, err := argoEndpoint(cfg.URL, apiPath, q)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, httpx.Internal(err)
		}
		rdr = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, argoRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := m.argoHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("argocd %s %s: %w", method, apiPath, err)
	}
	defer resp.Body.Close()
	data, err := readArgoBody(resp.Body, argoMaxBody)
	if err != nil {
		return nil, fmt.Errorf("argocd %s %s: %w", method, apiPath, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &argoStatusError{Method: method, Path: apiPath, Status: resp.StatusCode}
	}
	return data, nil
}

// ---------------------------------------------------------------- Argo API

const (
	argoUserInfoPath = "/api/v1/session/userinfo"
	argoAppsPath     = "/api/v1/applications"
)

func argoAppPath(app string) string { return argoAppsPath + "/" + app }

func argoSyncPath(app string) string { return argoAppPath(app) + "/sync" }

type argoAppSource struct {
	RepoURL        string `json:"repoURL"`
	TargetRevision string `json:"targetRevision"`
	Path           string `json:"path"`
	Chart          string `json:"chart"`
}

type argoApplication struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Project string          `json:"project"`
		Source  *argoAppSource  `json:"source"`
		Sources []argoAppSource `json:"sources"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status    string   `json:"status"`
			Revision  string   `json:"revision"`
			Revisions []string `json:"revisions"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
	} `json:"status"`
}

func (a *argoApplication) projectName() string {
	if a == nil {
		return ""
	}
	if p := strings.TrimSpace(a.Spec.Project); p != "" {
		return p
	}
	return "default"
}

func (a *argoApplication) targetRevision() string {
	if a == nil {
		return "HEAD"
	}
	if a.Spec.Source != nil {
		if r := strings.TrimSpace(a.Spec.Source.TargetRevision); r != "" {
			return r
		}
	}
	for _, s := range a.Spec.Sources {
		if r := strings.TrimSpace(s.TargetRevision); r != "" {
			return r
		}
	}
	if r := strings.TrimSpace(a.Status.Sync.Revision); r != "" {
		return r
	}
	return "HEAD"
}

func argoProjectOrDefault(project string) string {
	if p := strings.TrimSpace(project); p != "" {
		return p
	}
	return "default"
}

func validateArgoAppProject(cfg *argoConfig, app *argoApplication) error {
	if cfg == nil || app == nil {
		return httpx.Validation("argocd application is missing")
	}
	want := argoProjectOrDefault(cfg.Project)
	got := app.projectName()
	if got != want {
		return httpx.Forbidden(fmt.Sprintf("application %s belongs to project %q, outside configured project %q",
			app.Metadata.Name, got, want))
	}
	return nil
}

func resolveArgoRevision(app *argoApplication, requested string) (string, error) {
	if app == nil {
		return "", httpx.Validation("argocd application is missing")
	}
	if len(app.Spec.Sources) > 0 {
		return "", httpx.Validation("multi-source applications require revision-per-source synchronization in Argo CD")
	}
	rev := strings.TrimSpace(requested)
	if rev == "" {
		rev = strings.TrimSpace(app.Status.Sync.Revision)
	}
	if !validArgoRevision(rev) {
		return "", httpx.Validation("invalid argocd revision")
	}
	if app.Spec.Source != nil && app.Spec.Source.Chart != "" {
		if !argoChartVersionRe.MatchString(rev) {
			return "", httpx.Validation("Helm sync requires an exact chart version")
		}
	} else if !argoCommitRe.MatchString(rev) {
		return "", httpx.Validation("Git sync requires the full resolved commit SHA; refresh the application in Argo CD first")
	}
	return rev, nil
}

func argoSpecHash(raw json.RawMessage) (string, error) {
	var app struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(raw, &app); err != nil || app.Spec == nil {
		return "", httpx.Validation("application specification unavailable")
	}
	normalized, _ := json.Marshal(app)
	hash := sha256.Sum256(normalized)
	return hex.EncodeToString(hash[:]), nil
}

type argoUserInfo struct {
	LoggedIn bool   `json:"loggedIn"`
	Username string `json:"username"`
	Issuer   string `json:"iss"`
}

func (m *Module) argoUserinfo(ctx context.Context, cfg *argoConfig) (*argoUserInfo, error) {
	data, err := m.argoDo(ctx, cfg, http.MethodGet, argoUserInfoPath, nil, nil)
	if err != nil {
		return nil, err
	}
	var info argoUserInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("decode argocd userinfo: %w", err)
	}
	return &info, nil
}

func (m *Module) argoListApps(ctx context.Context, cfg *argoConfig) ([]json.RawMessage, error) {
	q := url.Values{}
	if project := argoProjectOrDefault(cfg.Project); project != "" {
		q.Add("projects", project)
	}
	data, err := m.argoDo(ctx, cfg, http.MethodGet, argoAppsPath, q, nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode argocd application list: %w", err)
	}
	items := make([]json.RawMessage, 0, len(list.Items))
	want := argoProjectOrDefault(cfg.Project)
	for _, raw := range list.Items {
		var app argoApplication
		if err := json.Unmarshal(raw, &app); err != nil {
			return nil, fmt.Errorf("decode argocd application list item: %w", err)
		}
		if app.projectName() != want {
			continue
		}
		items = append(items, raw)
	}
	return items, nil
}

func (m *Module) argoGetApplication(ctx context.Context, cfg *argoConfig, app string) (json.RawMessage, *argoApplication, error) {
	if !validArgoApp(app) {
		return nil, nil, httpx.Validation("invalid application name")
	}
	data, err := m.argoDo(ctx, cfg, http.MethodGet, argoAppPath(app), nil, nil)
	if err != nil {
		return nil, nil, err
	}
	var parsed argoApplication
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, nil, fmt.Errorf("decode argocd application: %w", err)
	}
	if parsed.Metadata.Name != app {
		return nil, nil, fmt.Errorf("argocd returned application %q for %q", parsed.Metadata.Name, app)
	}
	return json.RawMessage(data), &parsed, nil
}

type argoSyncStrategy struct {
	Apply argoSyncApply `json:"apply"`
}

type argoSyncApply struct {
	Force bool `json:"force"`
}

type argoSyncRequest struct {
	Name     string           `json:"name"`
	Revision string           `json:"revision"`
	Prune    bool             `json:"prune"`
	DryRun   bool             `json:"dryRun"`
	Strategy argoSyncStrategy `json:"strategy"`
}

func (m *Module) argoSyncApp(ctx context.Context, cfg *argoConfig, app, revision string) error {
	if !validArgoApp(app) {
		return httpx.Validation("invalid application name")
	}
	if !validArgoRevision(revision) {
		return httpx.Validation("invalid argocd revision")
	}
	body := argoSyncRequest{
		Name:     app,
		Revision: revision,
		Prune:    false,
		DryRun:   false,
		Strategy: argoSyncStrategy{Apply: argoSyncApply{Force: false}},
	}
	_, err := m.argoDo(ctx, cfg, http.MethodPost, argoSyncPath(app), nil, body)
	return err
}

// ---------------------------------------------------------------- HTTP routes

// registerArgoRoutes mounts the Argo CD endpoints on the authenticated cicd
// group and registers the sync approval type. The main module calls it from
// Register; it is not mounted automatically.
func (m *Module) registerArgoRoutes(g *gin.RouterGroup) {
	if g == nil || m.d == nil {
		return
	}
	if m.d.Approval != nil {
		if err := m.d.Approval.Register(approval.TypeSpec{
			Type: argoApprovalType, Module: "cicd", Description: "同步 Argo CD 应用",
			CreatePermission: "cicd:deploy", Timeout: 30 * time.Minute, Handler: &argoSyncHandler{m: m},
		}); err != nil && m.log != nil {
			m.log.Error("register argocd approval type", "err", err)
		}
	} else if m.log != nil {
		m.log.Warn("cicd: approval service is not configured; argocd sync will be unavailable")
	}

	read := rbac.RequirePerm("cicd:read")
	manage := rbac.RequirePerm("cicd:manage")
	deploy := rbac.RequirePerm("cicd:deploy")

	g.GET("/argocd", read, m.listArgoConfigs)
	g.POST("/argocd", manage, m.createArgoConfig)
	g.PUT("/argocd/:id", manage, m.updateArgoConfig)
	g.DELETE("/argocd/:id", manage, m.deleteArgoConfig)
	g.POST("/argocd/:id/test", manage, m.testArgoConfig)
	g.GET("/argocd/:id/applications", read, m.listArgoApplications)
	g.GET("/argocd/:id/applications/:app", read, m.getArgoApplication)
	g.POST("/argocd/:id/applications/:app/sync", deploy, m.syncArgoApplication)
}

func (m *Module) listArgoConfigs(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	var (
		rows pgx.Rows
		err  error
	)
	switch {
	case argoScopeAll(p.EnvScope):
		rows, err = m.db.Query(c, `SELECT `+argoCols+` FROM cicd_argocd ORDER BY id`)
	case len(p.EnvScope) == 0:
		httpx.OK(c, []*argoConfig{})
		return
	default:
		rows, err = m.db.Query(c, `SELECT `+argoCols+` FROM cicd_argocd WHERE env = ANY($1::text[]) ORDER BY id`, p.EnvScope)
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []*argoConfig{}
	for rows.Next() {
		cfg, err := scanArgoConfig(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		out = append(out, cfg.public())
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, out)
}

func (m *Module) createArgoConfig(c *gin.Context) {
	var in argoConfigIn
	if !httpx.Bind(c, &in) {
		return
	}
	if _, err := validateArgoConfigInput(&in, true); err != nil {
		httpx.Fail(c, err)
		return
	}
	if !requireArgoScope(c, in.Env) {
		return
	}
	change := cicdConfigMutation{Kind: "argocd.create", Argo: &in, Envs: []string{in.Env}}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	id, err := m.applyCICDConfigMutation(c, change)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("argocd config name exists").Wrap(errors.New("configuration write failed")))
		return
	}
	cfg, err := m.loadArgoConfig(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_argocd", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"env": cfg.Env, "project": cfg.Project})
	httpx.Created(c, cfg.public())
}

func (m *Module) updateArgoConfig(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	cur, err := m.loadArgoConfig(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	// Both the old and the new environment must be in the caller's scope: an
	// update must not be used to move a connection out of a restricted env.
	if !requireArgoScope(c, cur.Env) {
		return
	}
	var in argoConfigIn
	if !httpx.Bind(c, &in) {
		return
	}
	if _, err := validateArgoConfigInput(&in, false); err != nil {
		httpx.Fail(c, err)
		return
	}
	if !requireArgoScope(c, in.Env) {
		return
	}
	change := cicdConfigMutation{Kind: "argocd.update", ID: id, Argo: &in,
		ExistingSecretEnc: cur.tokenEnc, ExpectedUpdatedAt: &cur.UpdatedAt,
		Envs: []string{cur.Env, in.Env}}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	if _, err := m.applyCICDConfigMutation(c, change); err != nil {
		httpx.Fail(c, httpx.Conflict("argocd configuration changed or name exists").Wrap(errors.New("configuration write failed")))
		return
	}
	cfg, err := m.loadArgoConfig(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_argocd", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"env": cfg.Env, "project": cfg.Project})
	httpx.OK(c, cfg.public())
}

func (m *Module) deleteArgoConfig(c *gin.Context) {
	cfg, ok := m.argoConfigFromPath(c)
	if !ok {
		return
	}
	change := cicdConfigMutation{Kind: "argocd.delete", ID: cfg.ID, Envs: []string{cfg.Env}, ExpectedUpdatedAt: &cfg.UpdatedAt}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	if _, err := m.applyCICDConfigMutation(c, change); err != nil {
		httpx.Fail(c, httpx.Conflict("argocd config is in use or changed while approval was pending").Wrap(errors.New("configuration delete failed")))
		return
	}
	audit.SetResource(c, "cicd_argocd", fmt.Sprint(cfg.ID))
	httpx.OK(c, gin.H{"deleted": cfg.ID})
}

func (m *Module) testArgoConfig(c *gin.Context) {
	cfg, ok := m.argoConfigFromPath(c)
	if !ok {
		return
	}
	info, err := m.argoUserinfo(c, cfg)
	if err != nil {
		httpx.Fail(c, argoHTTPError(err))
		return
	}
	if cfg.tokenEnc != "" && !info.LoggedIn {
		httpx.Fail(c, httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "Argo CD 未接受配置的凭据"))
		return
	}
	audit.SetResource(c, "cicd_argocd", fmt.Sprint(cfg.ID))
	audit.SetDetail(c, gin.H{"username": info.Username, "logged_in": info.LoggedIn})
	httpx.OK(c, gin.H{"ok": true, "username": info.Username, "logged_in": info.LoggedIn})
}

func (m *Module) listArgoApplications(c *gin.Context) {
	cfg, ok := m.argoConfigFromPath(c)
	if !ok {
		return
	}
	items, err := m.argoListApps(c, cfg)
	if err != nil {
		httpx.Fail(c, argoHTTPError(err))
		return
	}
	audit.SetResource(c, "cicd_argocd", fmt.Sprint(cfg.ID))
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) getArgoApplication(c *gin.Context) {
	cfg, ok := m.argoConfigFromPath(c)
	if !ok {
		return
	}
	app := c.Param("app")
	raw, parsed, err := m.argoGetApplication(c, cfg, app)
	if err != nil {
		httpx.Fail(c, argoHTTPError(err))
		return
	}
	if err := validateArgoAppProject(cfg, parsed); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_argocd", fmt.Sprint(cfg.ID))
	audit.SetDetail(c, gin.H{"app": app, "project": parsed.projectName()})
	httpx.OK(c, raw)
}

// bindArgoSyncIn decodes an optional, size-limited sync request body. Argo CD
// clients commonly POST without a body, including with chunked transfer.
func bindArgoSyncIn(c *gin.Context) (argoSyncIn, bool) {
	var in argoSyncIn
	if c.Request.Body == nil {
		return in, true
	}
	b, err := readArgoBody(c.Request.Body, 64<<10)
	if err != nil {
		httpx.Fail(c, httpx.Validation("invalid request body"))
		return in, false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return in, true
	}
	if err := json.Unmarshal(b, &in); err != nil {
		httpx.Fail(c, httpx.Validation("invalid request body"))
		return in, false
	}
	return in, true
}

func (m *Module) syncArgoApplication(c *gin.Context) {
	cfg, ok := m.argoConfigFromPath(c)
	if !ok {
		return
	}
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	if !p.Can("cicd:deploy") {
		httpx.Fail(c, httpx.Forbidden("permission denied: cicd:deploy"))
		return
	}
	app := c.Param("app")
	if !validArgoApp(app) {
		httpx.Fail(c, httpx.Validation("invalid application name"))
		return
	}
	in, ok := bindArgoSyncIn(c)
	if !ok {
		return
	}
	in.Revision = strings.TrimSpace(in.Revision)
	if in.Revision != "" && !validArgoRevision(in.Revision) {
		httpx.Fail(c, httpx.Validation("invalid argocd revision"))
		return
	}
	if len(in.Reason) > 2000 {
		httpx.Fail(c, httpx.Validation("reason is too long"))
		return
	}
	// Validate the application and resolve the exact revision that will be
	// recorded in the approval payload (and used by the handler).
	rawApp, parsed, err := m.argoGetApplication(c, cfg, app)
	if err != nil {
		httpx.Fail(c, argoHTTPError(err))
		return
	}
	if err := validateArgoAppProject(cfg, parsed); err != nil {
		httpx.Fail(c, err)
		return
	}
	revision, err := resolveArgoRevision(parsed, in.Revision)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if m.d.Approval == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批服务不可用"))
		return
	}
	specHash, err := argoSpecHash(rawApp)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	t, pending, err := m.d.Approval.Gate(c, p, approval.CreateInput{
		Type:    argoApprovalType,
		Title:   fmt.Sprintf("同步 Argo CD 应用 %s → %s (%s)", cfg.Name, app, revision),
		Env:     cfg.Env,
		Reason:  in.Reason,
		Payload: argoSyncPayload{ConfigID: cfg.ID, App: app, Revision: revision, SpecHash: specHash, ConfigVersion: &cfg.UpdatedAt},
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_argocd", fmt.Sprint(cfg.ID))
	audit.SetDetail(c, gin.H{"app": app, "revision": revision})
	if pending {
		approval.RespondPending(c, t)
		return
	}
	if err := m.argoSyncApp(c, cfg, app, revision); err != nil {
		httpx.Fail(c, argoHTTPError(err))
		return
	}
	httpx.OK(c, argoSyncResult(cfg, app, revision))
}

func argoSyncResult(cfg *argoConfig, app, revision string) gin.H {
	return gin.H{
		"synced": true, "config_id": cfg.ID, "config_name": cfg.Name,
		"app": app, "revision": revision, "prune": false, "force": false,
	}
}

// ---------------------------------------------------------------- approval

type argoSyncHandler struct{ m *Module }

func (h *argoSyncHandler) OnApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	if h == nil || h.m == nil {
		return nil, httpx.Internal(errors.New("argocd sync handler is not configured"))
	}
	if t == nil {
		return nil, httpx.Validation("approval ticket is missing")
	}
	var p argoSyncPayload
	if err := t.DecodePayload(&p); err != nil {
		return nil, httpx.Validation("invalid approval payload")
	}
	if p.ConfigID <= 0 || !validArgoApp(p.App) || !validArgoRevision(p.Revision) {
		return nil, httpx.Validation("invalid approval payload")
	}
	if h.m.d == nil || h.m.db == nil {
		return nil, httpx.Internal(errors.New("cicd: database is not configured"))
	}
	// Reload the connection: its URL, token, environment and project may have
	// changed since the ticket was created.
	cfg, err := h.m.loadArgoConfig(ctx, p.ConfigID)
	if err != nil {
		return nil, err
	}
	if t.Env == "" || t.Env != cfg.Env {
		return nil, httpx.Conflict("argocd config environment does not match the approval ticket")
	}
	if err := h.authorize(ctx, t, cfg); err != nil {
		return nil, err
	}
	if p.ConfigVersion == nil || !p.ConfigVersion.Equal(cfg.UpdatedAt) || p.SpecHash == "" {
		return nil, httpx.Conflict("Argo CD connection changed; submit a new sync request")
	}
	rawApp, parsed, err := h.m.argoGetApplication(ctx, cfg, p.App)
	if err != nil {
		return nil, argoHTTPError(err)
	}
	if err := validateArgoAppProject(cfg, parsed); err != nil {
		return nil, err
	}
	hash, err := argoSpecHash(rawApp)
	if err != nil || hash != p.SpecHash {
		return nil, httpx.Conflict("application identity or specification changed after approval was requested")
	}
	if _, err := resolveArgoRevision(parsed, p.Revision); err != nil {
		return nil, err
	}
	if err := h.m.argoSyncApp(ctx, cfg, p.App, p.Revision); err != nil {
		return nil, argoHTTPError(err)
	}
	return argoSyncResult(cfg, p.App, p.Revision), nil
}

// authorize re-resolves the applicant's current permissions and environment
// scope. The approval service executes handlers with a bare principal, so the
// permission check must be done against RBAC state rather than the ticket.
func (h *argoSyncHandler) authorize(ctx context.Context, t *approval.Ticket, cfg *argoConfig) error {
	if h == nil || h.m == nil || t == nil || cfg == nil {
		return httpx.Internal(errors.New("argocd sync handler is not configured"))
	}
	p := rbac.FromContext(ctx)
	userID, username := t.ApplicantID, t.ApplicantName
	if p != nil && p.UserID != 0 && p.UserID != userID {
		return httpx.Forbidden("approval applicant does not match the execution principal")
	}
	if userID <= 0 {
		return httpx.Unauthorized("approval applicant is missing")
	}
	var perms, scope []string
	switch {
	case h.m.d != nil && h.m.d.RBAC != nil:
		_, ps, es, err := h.m.d.RBAC.UserAccess(ctx, userID)
		if err != nil {
			return err
		}
		perms, scope = ps, es
	case p != nil && len(p.Permissions) > 0:
		// Only a caller that already carries permissions (for example a test
		// harness) may use this fallback; the approval service's synthetic
		// principal carries none.
		perms, scope = p.Permissions, p.EnvScope
	default:
		return httpx.Internal(errors.New("cicd: RBAC is not configured"))
	}
	principal := &rbac.Principal{UserID: userID, Username: username, Permissions: perms, EnvScope: scope}
	if !principal.Can("cicd:deploy") {
		return httpx.Forbidden("permission denied: cicd:deploy")
	}
	if cfg.Env == "" || !principal.CanEnv(cfg.Env) {
		return httpx.Forbidden("environment not in your scope: " + cfg.Env)
	}
	return nil
}
