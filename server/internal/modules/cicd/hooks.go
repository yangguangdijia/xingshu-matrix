package cicd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// ---------------------------------------------------------------- webhooks

// verifyWebhook authenticates a GitLab (X-Gitlab-Token) or GitHub
// (X-Hub-Signature-256 HMAC) delivery and returns the provider name.
func verifyWebhook(h http.Header, body []byte, secret string) (string, error) {
	if tok := h.Get("X-Gitlab-Token"); tok != "" {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) == 1 {
			return "gitlab", nil
		}
		return "", errors.New("bad gitlab token")
	}
	if sig := h.Get("X-Hub-Signature-256"); strings.HasPrefix(sig, "sha256=") {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := mac.Sum(nil)
		got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
		if err == nil && hmac.Equal(got, want) {
			return "github", nil
		}
		return "", errors.New("bad github signature")
	}
	return "", errors.New("missing X-Gitlab-Token or X-Hub-Signature-256")
}

type pushEvent struct {
	Ref         string `json:"ref"`
	After       string `json:"after"`
	CheckoutSHA string `json:"checkout_sha"`
	Deleted     bool   `json:"deleted"`
}

func (m *Module) webhook(c *gin.Context) {
	appID, err := strconv.ParseInt(c.Param("appId"), 10, 64)
	if err != nil {
		httpx.Fail(c, httpx.NotFound("application not found"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("read body"))
		return
	}
	a, err := m.loadApp(c, appID)
	if err != nil || a.webhookEnc == "" {
		httpx.Fail(c, httpx.Unauthorized("invalid webhook"))
		return
	}
	secret, err := m.d.Crypto.Decrypt(a.webhookEnc)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	provider, err := verifyWebhook(c.Request.Header, body, secret)
	if err != nil {
		m.log.Warn("webhook rejected", "app", appID, "err", err, "ip", c.ClientIP())
		httpx.Fail(c, httpx.Unauthorized("invalid webhook signature"))
		return
	}
	event := c.GetHeader("X-GitHub-Event") + c.GetHeader("X-Gitlab-Event")
	if event != "push" && event != "Push Hook" {
		httpx.OK(c, gin.H{"ignored": "event " + event})
		return
	}
	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		httpx.Fail(c, httpx.BadRequest("invalid push payload"))
		return
	}
	sha := ev.After
	if ev.CheckoutSHA != "" {
		sha = ev.CheckoutSHA
	}
	branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
	if ev.Deleted || strings.Trim(sha, "0") == "" || branch != a.Branch {
		httpx.OK(c, gin.H{"ignored": "ref " + ev.Ref})
		return
	}
	rows, err := m.db.Query(c, `SELECT id FROM cicd_pipelines WHERE app_id=$1 AND on_push ORDER BY id`, appID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	runs := []int64{}
	for _, id := range ids {
		r, err := m.startRun(c, nil, id, runIn{Branch: branch, Commit: sha}, "webhook:"+provider)
		if err != nil {
			m.log.Error("webhook start run", "pipeline", id, "err", err)
			continue
		}
		runs = append(runs, r.ID)
	}
	httpx.OK(c, gin.H{"runs": runs})
}

// ---------------------------------------------------------------- Jenkins

type jenkins struct {
	base, user, token string
	hc                *http.Client
}

func newJenkins(base, user, token string) *jenkins {
	jar, _ := cookiejar.New(nil) // crumbs are bound to the session cookie
	return &jenkins{base: strings.TrimRight(base, "/"), user: user, token: token, hc: &http.Client{Timeout: 60 * time.Second, Jar: jar}}
}

func jobPath(name string) string {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	for i, p := range parts {
		parts[i] = "job/" + url.PathEscape(p)
	}
	return "/" + strings.Join(parts, "/")
}

func validJenkinsJob(name string) bool {
	if name == "" || len(name) > 512 || strings.TrimSpace(name) != name || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\?#`) {
			return false
		}
		for _, r := range part {
			if r < 0x20 || r == 0x7f {
				return false
			}
		}
	}
	return true
}

type jenkinsParameter struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type,omitempty"`
	Default     string   `json:"default_value,omitempty"`
	Choices     []string `json:"choices,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"`
}

var jenkinsParameterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

func (j *jenkins) parameters(ctx context.Context, job string) ([]jenkinsParameter, error) {
	if !validJenkinsJob(job) {
		return nil, errors.New("invalid Jenkins job path")
	}
	tree := url.Values{}
	tree.Set("tree", "property[parameterDefinitions[name,description,type,defaultParameterValue[value],choices]]")
	var response struct {
		Property []struct {
			Parameters []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Type        string          `json:"type"`
				Default     json.RawMessage `json:"defaultParameterValue"`
				Choices     json.RawMessage `json:"choices"`
			} `json:"parameterDefinitions"`
		} `json:"property"`
	}
	if err := j.getJSON(ctx, jobPath(job)+"/api/json?"+tree.Encode(), &response); err != nil {
		return nil, err
	}
	out := make([]jenkinsParameter, 0, 16)
	seen := map[string]bool{}
	for _, property := range response.Property {
		for _, definition := range property.Parameters {
			if !jenkinsParameterName.MatchString(definition.Name) || seen[definition.Name] {
				continue
			}
			seen[definition.Name] = true
			item := jenkinsParameter{Name: definition.Name, Description: definition.Description, Type: definition.Type}
			lower := strings.ToLower(definition.Name + " " + definition.Type)
			item.Sensitive = strings.Contains(lower, "password") || strings.Contains(lower, "secret") ||
				strings.Contains(lower, "token") || strings.Contains(lower, "credential")
			if !item.Sensitive && len(definition.Default) > 0 && string(definition.Default) != "null" {
				var defaultValue any
				var wrapped struct {
					Value any `json:"value"`
				}
				if json.Unmarshal(definition.Default, &wrapped) == nil && wrapped.Value != nil {
					defaultValue = wrapped.Value
				}
				if defaultValue != nil {
					item.Default = fmt.Sprint(defaultValue)
				}
			}
			if !item.Sensitive && len(definition.Choices) > 0 && string(definition.Choices) != "null" {
				var choices []string
				if json.Unmarshal(definition.Choices, &choices) == nil {
					if len(choices) > 100 {
						choices = choices[:100]
					}
					item.Choices = choices
				} else {
					var block string
					if json.Unmarshal(definition.Choices, &block) == nil {
						choices = strings.Split(block, "\n")
						if len(choices) > 100 {
							choices = choices[:100]
						}
						item.Choices = choices
					}
				}
			}
			out = append(out, item)
			if len(out) >= 200 {
				return out, nil
			}
		}
	}
	return out, nil
}

func (j *jenkins) do(ctx context.Context, method, u string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	if !strings.HasPrefix(u, "http") {
		u = j.base + u
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if j.user != "" {
		req.SetBasicAuth(j.user, j.token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return j.hc.Do(req)
}

func (j *jenkins) getJSON(ctx context.Context, u string, v any) error {
	resp, err := j.do(ctx, http.MethodGet, u, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jenkins GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// trigger queues a build and returns the queue item URL.
func (j *jenkins) trigger(ctx context.Context, job string, params map[string]string) (string, error) {
	if !validJenkinsJob(job) {
		return "", errors.New("invalid Jenkins job path")
	}
	hdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	var crumb struct {
		Crumb             string `json:"crumb"`
		CrumbRequestField string `json:"crumbRequestField"`
	}
	if err := j.getJSON(ctx, "/crumbIssuer/api/json", &crumb); err == nil && crumb.Crumb != "" {
		hdr[crumb.CrumbRequestField] = crumb.Crumb
	} // 404 = CSRF protection disabled
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	ep := jobPath(job) + "/buildWithParameters"
	if len(params) == 0 {
		ep = jobPath(job) + "/build"
	}
	resp, err := j.do(ctx, http.MethodPost, ep, strings.NewReader(form.Encode()), hdr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("jenkins trigger %s: %s %s", job, resp.Status, bytes.TrimSpace(b))
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("jenkins returned no queue location")
	}
	return strings.TrimRight(loc, "/") + "/", nil
}

// waitExecutable polls a queue item until it becomes a build.
func (j *jenkins) waitExecutable(ctx context.Context, queueURL string) (string, error) {
	for {
		var q struct {
			Cancelled  bool   `json:"cancelled"`
			Why        string `json:"why"`
			Executable *struct {
				URL string `json:"url"`
			} `json:"executable"`
		}
		if err := j.getJSON(ctx, queueURL+"api/json", &q); err != nil {
			return "", err
		}
		if q.Cancelled {
			return "", errors.New("jenkins queue item cancelled")
		}
		if q.Executable != nil && q.Executable.URL != "" {
			return strings.TrimRight(q.Executable.URL, "/") + "/", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// follow streams progressive console text and returns the build result.
func (j *jenkins) follow(ctx context.Context, buildURL string, logf func(string, ...any)) (string, error) {
	start := "0"
	for {
		resp, err := j.do(ctx, http.MethodGet, buildURL+"logText/progressiveText?start="+start, nil, nil)
		if err != nil {
			if ctx.Err() != nil {
				_, _ = j.do(context.Background(), http.MethodPost, buildURL+"stop", nil, nil)
			}
			return "", err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if l != "" {
				logf("%s", l)
			}
		}
		if s := resp.Header.Get("X-Text-Size"); s != "" {
			start = s
		}
		if resp.Header.Get("X-More-Data") != "true" {
			var st struct {
				Building bool   `json:"building"`
				Result   string `json:"result"`
			}
			if err := j.getJSON(ctx, buildURL+"api/json", &st); err != nil {
				return "", err
			}
			if !st.Building {
				return st.Result, nil
			}
		}
		select {
		case <-ctx.Done():
			_, _ = j.do(context.Background(), http.MethodPost, buildURL+"stop", nil, nil)
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Module) jenkinsStage(ctx context.Context, run *Run, a *Application, s Stage, logf func(string, ...any)) error {
	j, err := m.jenkinsClient(ctx, s.JenkinsID)
	if err != nil {
		return err
	}
	params := map[string]string{}
	for k, v := range s.Params {
		v = strings.NewReplacer("${BRANCH}", run.Branch, "${COMMIT}", run.Commit, "${IMAGE_TAG}", run.ImageTag,
			"${IMAGE}", a.ImageRepo+":"+run.ImageTag, "${RUN_ID}", fmt.Sprint(run.ID)).Replace(v)
		params[k] = v
	}
	q, err := j.trigger(ctx, s.JenkinsJob, params)
	if err != nil {
		return err
	}
	logf("jenkins job %s queued: %s", s.JenkinsJob, q)
	b, err := j.waitExecutable(ctx, q)
	if err != nil {
		return err
	}
	logf("jenkins build started: %s", b)
	res, err := j.follow(ctx, b, logf)
	if err != nil {
		return err
	}
	logf("jenkins result: %s", res)
	if res != "SUCCESS" {
		return fmt.Errorf("jenkins build %s", strings.ToLower(res))
	}
	return nil
}
