package cicd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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

// giteeSignatureSkew bounds how far a Gitee signature timestamp may drift
// from the server clock.
const giteeSignatureSkew = time.Hour

// verifyWebhook authenticates a GitLab (X-Gitlab-Token), Gitee (X-Gitee-Token:
// the plain WebHook password, or a signature when X-Gitee-Timestamp is sent)
// or GitHub / Gitea / Gogs (X-Hub-Signature-256 HMAC) delivery and returns
// the provider name.
func verifyWebhook(h http.Header, body []byte, secret string) (string, error) {
	return verifyWebhookAt(h, body, secret, time.Now())
}

func verifyWebhookAt(h http.Header, body []byte, secret string, now time.Time) (string, error) {
	if secret == "" {
		return "", errors.New("webhook secret not configured")
	}
	if tok := h.Get("X-Gitlab-Token"); tok != "" {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) == 1 {
			return "gitlab", nil
		}
		return "", errors.New("bad gitlab token")
	}
	if tok := h.Get("X-Gitee-Token"); tok != "" {
		if ts := strings.TrimSpace(h.Get("X-Gitee-Timestamp")); ts != "" {
			if verifyGiteeSignature(tok, ts, secret, now) {
				return "gitee", nil
			}
			return "", errors.New("bad gitee signature")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) == 1 {
			return "gitee", nil
		}
		return "", errors.New("bad gitee token")
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
	return "", errors.New("missing X-Gitlab-Token, X-Gitee-Token or X-Hub-Signature-256")
}

// verifyGiteeSignature checks a Gitee signed delivery:
// token = base64(HMAC-SHA256(secret, timestamp + "\n" + secret)), timestamp in
// milliseconds. Gitee may send the token URL-encoded.
func verifyGiteeSignature(token, ts, secret string, now time.Time) bool {
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.UnixMilli(ms)); d > giteeSignatureSkew || d < -giteeSignatureSkew {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	want := mac.Sum(nil)
	for _, cand := range []string{token, unescapeOrEmpty(token)} {
		if cand == "" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(cand)
		if err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

func unescapeOrEmpty(s string) string {
	v, err := url.QueryUnescape(s)
	if err != nil || v == s {
		return ""
	}
	return v
}

// webhookEvent returns the event name of a delivery (first provider header set).
func webhookEvent(h http.Header) string {
	for _, k := range []string{"X-GitHub-Event", "X-Gitlab-Event", "X-Gitee-Event", "X-Gitea-Event", "X-Gogs-Event"} {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

// isPushEvent accepts GitHub/Gitea/Gogs "push" and GitLab/Gitee "Push Hook".
func isPushEvent(event string) bool {
	return event == "push" || event == "Push Hook"
}

type pushEvent struct {
	Ref         string       `json:"ref"`
	After       string       `json:"after"`
	CheckoutSHA string       `json:"checkout_sha"`
	Deleted     bool         `json:"deleted"`
	Commits     []pushCommit `json:"commits"`
}

// pushCommit is one commit of a push payload (GitHub / GitLab / Gitee / Gitea share these fields).
type pushCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	URL     string `json:"url"`
	Author  struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	} `json:"author"`
}

// commitsJSON keeps the newest 20 commits of a push ({sha,message,author,url}, message <= 1000 bytes)
// so the devcenter value stream can link REQ-/BUG- keys in commit messages to runs.
func commitsJSON(cs []pushCommit) []byte {
	if len(cs) > 20 {
		cs = cs[len(cs)-20:]
	}
	type item struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Author  string `json:"author"`
		URL     string `json:"url"`
	}
	out := make([]item, 0, len(cs))
	for _, c := range cs {
		msg := c.Message
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		author := c.Author.Name
		if author == "" {
			author = c.Author.Username
		}
		if len(c.ID) > 64 || len(c.URL) > 500 {
			continue
		}
		out = append(out, item{SHA: c.ID, Message: msg, Author: author, URL: c.URL})
	}
	b, _ := json.Marshal(out)
	return b
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
	event := webhookEvent(c.Request.Header)
	keyProvider, deliveryID := deliveryKey(c.Request.Header)
	if keyProvider == "" {
		keyProvider = provider
	}

	// Parse the push payload before claiming so the delivery row carries ref/commit.
	var ev pushEvent
	payloadErr := error(nil)
	if isPushEvent(event) {
		if err := json.Unmarshal(body, &ev); err != nil {
			payloadErr = err
		}
	}
	sha := ev.After
	if ev.CheckoutSHA != "" {
		sha = ev.CheckoutSHA
	}
	if len(sha) > 64 {
		sha = sha[:64]
	}
	ref := ev.Ref
	if len(ref) > 255 {
		ref = ref[:255]
	}
	if len(event) > 64 {
		event = event[:64]
	}

	rowID, duplicate, err := m.claimDelivery(c, appID, keyProvider, deliveryID, event, ref, sha)
	if err != nil {
		m.log.Error("webhook claim delivery", "app", appID, "err", err)
		httpx.Fail(c, err)
		return
	}
	if duplicate {
		m.log.Info("webhook duplicate delivery ignored", "app", appID, "provider", keyProvider, "delivery", deliveryID)
		httpx.OK(c, gin.H{"duplicate": true, "delivery_id": deliveryID, "runs": []int64{}})
		return
	}

	commits := commitsJSON(ev.Commits)
	if len(ev.Commits) > 0 {
		if _, err := m.db.Exec(c, `UPDATE cicd_webhook_deliveries SET commits=$2 WHERE id=$1`, rowID, commits); err != nil {
			m.log.Warn("webhook store commits", "app", appID, "err", err)
		}
	}

	if !isPushEvent(event) {
		m.finishDelivery(rowID, "ignored", "event "+event, nil)
		httpx.OK(c, gin.H{"ignored": "event " + event})
		return
	}
	if payloadErr != nil {
		m.finishDelivery(rowID, "error", "invalid push payload", nil)
		httpx.Fail(c, httpx.BadRequest("invalid push payload"))
		return
	}
	branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
	if ev.Deleted || strings.Trim(sha, "0") == "" || branch != a.Branch {
		m.finishDelivery(rowID, "ignored", "ref "+ev.Ref, nil)
		httpx.OK(c, gin.H{"ignored": "ref " + ev.Ref})
		return
	}
	rows, err := m.db.Query(c, `SELECT id FROM cicd_pipelines WHERE app_id=$1 AND on_push ORDER BY id`, appID)
	if err != nil {
		m.finishDelivery(rowID, "error", err.Error(), nil)
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
	if len(ids) == 0 {
		m.finishDelivery(rowID, "ignored", "no pipeline triggers on push", nil)
		httpx.OK(c, gin.H{"runs": []int64{}, "ignored": "no pipeline triggers on push"})
		return
	}
	runs := []int64{}
	refs := []WebhookRunRef{}
	var failures []string
	for _, id := range ids {
		r, err := m.startRun(c, nil, id, runIn{Branch: branch, Commit: sha}, "webhook:"+provider)
		if err != nil {
			m.log.Error("webhook start run", "pipeline", id, "err", err)
			failures = append(failures, fmt.Sprintf("pipeline %d: %v", id, err))
			continue
		}
		if len(ev.Commits) > 0 {
			if _, err := m.db.Exec(c, `UPDATE cicd_runs SET commits=$2 WHERE id=$1`, r.ID, commits); err != nil {
				m.log.Warn("webhook store run commits", "run", r.ID, "err", err)
			}
		}
		runs = append(runs, r.ID)
		refs = append(refs, WebhookRunRef{PipelineID: id, RunID: r.ID})
	}
	switch {
	case len(refs) == 0:
		// Nothing started: a redelivery is processed again.
		m.finishDelivery(rowID, "error", strings.Join(failures, "; "), nil)
	default:
		m.finishDelivery(rowID, "triggered", strings.Join(failures, "; "), refs)
	}
	httpx.OK(c, gin.H{"runs": runs})
}

// pruneDeliveriesLoop removes expired webhook deliveries hourly.
func (m *Module) pruneDeliveriesLoop(ctx context.Context) {
	m.pruneDeliveries(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.pruneDeliveries(ctx)
		}
	}
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
