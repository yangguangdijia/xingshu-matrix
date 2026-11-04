package cicd

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// 凭证管理：Git Token / 账号密码 / SSH 私钥 / 镜像仓库账号统一保存（平台密钥加密），
// 应用按 credential_id 引用 Git 凭证，构建阶段按 registry_credential_id 引用镜像仓库凭证。
// 密文永不返回给前端；运行时只写入本次运行的临时 Secret，运行结束即删除。

// Credential is a reusable secret for Git or an image registry.
type Credential struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name" binding:"required,max=63"`
	Type        string    `json:"type" binding:"required"`
	Username    string    `json:"username" binding:"max=256"`
	Secret      string    `json:"secret,omitempty" binding:"max=16384"` // input only
	Host        string    `json:"host" binding:"max=256"`
	Description string    `json:"description" binding:"max=512"`
	HasSecret   bool      `json:"has_secret"`
	UsedBy      int       `json:"used_by"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	secretEnc string
}

var credentialTypes = map[string]bool{"git_token": true, "git_basic": true, "ssh_key": true, "registry": true}
var credentialName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var registryHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

func isGitCredential(t string) bool { return t == "git_token" || t == "git_basic" || t == "ssh_key" }

const credCols = `id,name,type,username,secret_enc,host,description,created_by,created_at,updated_at`

func scanCredential(row pgx.Row) (*Credential, error) {
	var c Credential
	if err := row.Scan(&c.ID, &c.Name, &c.Type, &c.Username, &c.secretEnc, &c.Host, &c.Description, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	c.HasSecret = c.secretEnc != ""
	return &c, nil
}

func (m *Module) loadCredential(ctx context.Context, id int64) (*Credential, error) {
	return scanCredential(m.db.QueryRow(ctx, `SELECT `+credCols+` FROM cicd_credentials WHERE id=$1`, id))
}

// credentialSecret returns the decrypted secret of a credential.
func (m *Module) credentialSecret(c *Credential) (string, error) {
	if m.d == nil || m.d.Crypto == nil {
		return "", errors.New("crypto is not configured")
	}
	return m.d.Crypto.Decrypt(c.secretEnc)
}

func (m *Module) credentialUsage(ctx context.Context, id int64) (int, error) {
	var apps, pipes int
	if err := m.db.QueryRow(ctx, `SELECT count(*) FROM cicd_applications WHERE credential_id=$1`, id).Scan(&apps); err != nil {
		return 0, err
	}
	ref, _ := json.Marshal([]map[string]int64{{"registry_credential_id": id}})
	if err := m.db.QueryRow(ctx, `SELECT count(*) FROM cicd_pipelines WHERE stages @> $1::jsonb`, string(ref)).Scan(&pipes); err != nil {
		return 0, err
	}
	return apps + pipes, nil
}

func validateCredential(in *Credential, creating bool) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.Username = strings.TrimSpace(in.Username)
	if !credentialName.MatchString(in.Name) {
		return httpx.Validation("name must be a DNS label (lowercase letters, digits, '-')")
	}
	if !credentialTypes[in.Type] {
		return httpx.Validation("type must be one of git_token, git_basic, ssh_key, registry")
	}
	if creating && in.Secret == "" {
		return httpx.Validation("secret is required")
	}
	switch in.Type {
	case "git_basic":
		if in.Username == "" {
			return httpx.Validation("username is required for git_basic")
		}
	case "registry":
		if in.Username == "" {
			return httpx.Validation("username is required for registry credentials")
		}
		if !registryHost.MatchString(in.Host) {
			return httpx.Validation("host must be a registry host such as ghcr.io or 10.0.0.5:5000")
		}
	case "ssh_key":
		if in.Secret != "" && !strings.Contains(in.Secret, "-----BEGIN") {
			return httpx.Validation("secret must be a PEM / OpenSSH private key")
		}
	}
	return nil
}

func (m *Module) listCredentials(c *gin.Context) {
	rows, err := m.db.Query(c, `SELECT `+credCols+` FROM cicd_credentials ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []*Credential{}
	for rows.Next() {
		cr, err := scanCredential(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		items = append(items, cr)
	}
	rows.Close()
	for _, cr := range items {
		cr.UsedBy, _ = m.credentialUsage(c, cr.ID)
	}
	if t := c.Query("type"); t != "" {
		filtered := items[:0]
		for _, cr := range items {
			if cr.Type == t || (t == "git" && isGitCredential(cr.Type)) {
				filtered = append(filtered, cr)
			}
		}
		items = filtered
	}
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) createCredential(c *gin.Context) {
	var in Credential
	if !httpx.Bind(c, &in) {
		return
	}
	if err := validateCredential(&in, true); err != nil {
		httpx.Fail(c, err)
		return
	}
	if m.d == nil || m.d.Crypto == nil {
		httpx.Fail(c, httpx.Internal(errors.New("crypto is not configured")))
		return
	}
	enc, err := m.d.Crypto.Encrypt(in.Secret)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	by := ""
	if p := rbac.PrincipalFrom(c); p != nil {
		by = p.Username
	}
	var id int64
	err = m.db.QueryRow(c, `INSERT INTO cicd_credentials(name,type,username,secret_enc,host,description,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`, in.Name, in.Type, in.Username, enc, in.Host, in.Description, by).Scan(&id)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("credential name exists"))
		return
	}
	audit.SetResource(c, "cicd_credential", fmt.Sprint(id))
	out, err := m.loadCredential(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.Created(c, out)
}

func (m *Module) updateCredential(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	cur, err := m.loadCredential(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var in Credential
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Type != cur.Type {
		httpx.Fail(c, httpx.Validation("credential type cannot be changed"))
		return
	}
	if err := validateCredential(&in, false); err != nil {
		httpx.Fail(c, err)
		return
	}
	enc := cur.secretEnc
	if in.Secret != "" {
		if enc, err = m.d.Crypto.Encrypt(in.Secret); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	if _, err := m.db.Exec(c, `UPDATE cicd_credentials SET name=$2,username=$3,secret_enc=$4,host=$5,description=$6,updated_at=now() WHERE id=$1`,
		id, in.Name, in.Username, enc, in.Host, in.Description); err != nil {
		httpx.Fail(c, httpx.Conflict("credential name exists"))
		return
	}
	audit.SetResource(c, "cicd_credential", fmt.Sprint(id))
	out, err := m.loadCredential(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, out)
}

func (m *Module) deleteCredential(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	n, err := m.credentialUsage(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if n > 0 {
		httpx.Fail(c, httpx.Conflict("credential is in use"))
		return
	}
	tag, err := m.db.Exec(c, `DELETE FROM cicd_credentials WHERE id=$1`, id)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("credential is in use"))
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.ErrNotFound)
		return
	}
	audit.SetResource(c, "cicd_credential", fmt.Sprint(id))
	httpx.OK(c, gin.H{"deleted": id})
}

// validateCredentialRef checks that id names a credential of an allowed type.
func (m *Module) validateCredentialRef(ctx context.Context, id int64, git bool) (*Credential, error) {
	if id == 0 {
		return nil, nil
	}
	cr, err := m.loadCredential(ctx, id)
	if err != nil {
		return nil, httpx.Validation("unknown credential")
	}
	if git && !isGitCredential(cr.Type) {
		return nil, httpx.Validation("credential is not a Git credential")
	}
	if !git && cr.Type != "registry" {
		return nil, httpx.Validation("credential is not a registry credential")
	}
	return cr, nil
}

// validatePipelineCredentials checks registry_credential_id on build stages.
func (m *Module) validatePipelineCredentials(ctx context.Context, stages []Stage) error {
	for _, s := range stages {
		if s.RegistryCredentialID == 0 {
			continue
		}
		if s.Type != "build" {
			return httpx.Validation("registry_credential_id is only valid on build stages")
		}
		if _, err := m.validateCredentialRef(ctx, s.RegistryCredentialID, false); err != nil {
			return err
		}
	}
	return nil
}

// dockerConfigJSON renders a kubernetes.io/dockerconfigjson payload.
func dockerConfigJSON(host, user, secret string) ([]byte, error) {
	if host == "docker.io" || host == "index.docker.io" {
		host = "https://index.docker.io/v1/"
	}
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
	return json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"username": user, "password": secret, "auth": auth}}})
}

// ---------------------------------------------------------------- repo probe

// RepoProbe is the result of POST /cicd/repos/probe.
type RepoProbe struct {
	Branches      []string `json:"branches"`
	DefaultBranch string   `json:"default_branch"`
	Head          string   `json:"head"`
}

const (
	probeTimeout     = 10 * time.Second
	probeMaxBytes    = 4 << 20
	probeMaxBranches = 1000
)

var errProbeAuth = errors.New("authentication failed")

func (m *Module) probeRepo(c *gin.Context) {
	var in struct {
		RepoURL      string `json:"repo_url" binding:"required,max=2048"`
		CredentialID int64  `json:"credential_id"`
		AppID        int64  `json:"app_id"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if err := validateApplicationRepoURL(in.RepoURL); err != nil {
		httpx.Fail(c, err)
		return
	}
	user, pass := "", ""
	credID := in.CredentialID
	if credID == 0 && in.AppID != 0 {
		if a, err := m.loadApp(c, in.AppID); err == nil && a.RepoURL == in.RepoURL {
			credID = a.CredentialID
			if credID == 0 && a.gitTokenEnc != "" {
				tok, err := m.d.Crypto.Decrypt(a.gitTokenEnc)
				if err == nil {
					user, pass = a.GitUsername, tok
					if user == "" {
						user = "oauth2"
					}
				}
			}
		}
	}
	if credID != 0 {
		cr, err := m.validateCredentialRef(c, credID, true)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if cr.Type == "ssh_key" {
			httpx.Fail(c, httpx.Validation("SSH repositories cannot be probed from the server; branches are read at run time"))
			return
		}
		secret, err := m.credentialSecret(cr)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		user, pass = cr.Username, secret
		if user == "" {
			user = "oauth2"
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), probeTimeout)
	defer cancel()
	res, err := probeGitRefs(ctx, strings.TrimSpace(in.RepoURL), user, pass)
	if err != nil {
		if errors.Is(err, errProbeAuth) {
			httpx.Fail(c, httpx.Validation("authentication failed: check the credential"))
			return
		}
		var he *httpx.Error
		if errors.As(err, &he) {
			httpx.Fail(c, he)
			return
		}
		httpx.Fail(c, httpx.Validation("cannot read repository: "+redactCICDText(err.Error())))
		return
	}
	httpx.OK(c, res)
}

func probeGitRefs(ctx context.Context, repo, user, pass string) (*RepoProbe, error) {
	if strings.HasPrefix(repo, "git@") || strings.HasPrefix(repo, "ssh://") {
		return nil, httpx.Validation("SSH repositories cannot be probed from the server; branches are read at run time")
	}
	u, err := url.Parse(repo)
	if err != nil || u.Host == "" {
		return nil, httpx.Validation("invalid repository URL")
	}
	switch u.Scheme {
	case "http", "https":
		return probeSmartHTTP(ctx, u, user, pass)
	case "git":
		return probeGitDaemon(ctx, u)
	default:
		return nil, httpx.Validation("unsupported repository scheme " + u.Scheme)
	}
}

func probeSmartHTTP(ctx context.Context, u *url.URL, user, pass string) (*RepoProbe, error) {
	target := strings.TrimSuffix(u.String(), "/") + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "git/2.45 matrix-cicd")
	if pass != "" {
		req.SetBasicAuth(user, pass)
	}
	client := &http.Client{Timeout: probeTimeout, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if pass != "" && r.URL.Host != u.Host {
			return errors.New("redirected to another host")
		}
		if pass != "" {
			r.SetBasicAuth(user, pass)
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, errProbeAuth
	case resp.StatusCode == http.StatusNotFound:
		return nil, errors.New("repository not found")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	return parseRefAdvertisement(io.LimitReader(resp.Body, probeMaxBytes), true)
}

func probeGitDaemon(ctx context.Context, u *url.URL) (*RepoProbe, error) {
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "9418")
	}
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := conn.Write(pktLine("git-upload-pack " + u.EscapedPath() + "\x00host=" + u.Host + "\x00")); err != nil {
		return nil, err
	}
	res, err := parseRefAdvertisement(io.LimitReader(conn, probeMaxBytes), false)
	_, _ = conn.Write([]byte("0000"))
	return res, err
}

func pktLine(s string) []byte { return []byte(fmt.Sprintf("%04x%s", len(s)+4, s)) }

// parseRefAdvertisement reads a git ref advertisement (pkt-line format).
// For smart HTTP the stream starts with "# service=git-upload-pack" + flush.
func parseRefAdvertisement(r io.Reader, smartHTTP bool) (*RepoProbe, error) {
	br := bufio.NewReader(r)
	read := func() (string, bool, error) {
		var hdr [4]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return "", false, err
		}
		n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
		if err != nil {
			return "", false, errors.New("not a git server response")
		}
		if n == 0 {
			return "", true, nil
		}
		if n < 4 || n > 65520 {
			return "", false, errors.New("invalid pkt-line")
		}
		buf := make([]byte, n-4)
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", false, err
		}
		return strings.TrimSuffix(string(buf), "\n"), false, nil
	}
	if smartHTTP {
		line, _, err := read()
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(line, "# service=") {
			return nil, errors.New("not a git smart-HTTP endpoint")
		}
		for {
			_, flush, err := read()
			if err != nil {
				return nil, err
			}
			if flush {
				break
			}
		}
	}
	res := &RepoProbe{Branches: []string{}}
	headSHA, first := "", true
	shas := map[string]string{}
	for {
		line, flush, err := read()
		if err != nil {
			if errors.Is(err, io.EOF) && !first {
				break
			}
			return nil, err
		}
		if flush {
			break
		}
		if strings.HasPrefix(line, "ERR ") {
			return nil, errors.New(strings.TrimPrefix(line, "ERR "))
		}
		if first {
			first = false
			if i := strings.IndexByte(line, 0); i >= 0 {
				for _, cap := range strings.Fields(line[i+1:]) {
					if strings.HasPrefix(cap, "symref=HEAD:refs/heads/") {
						res.DefaultBranch = strings.TrimPrefix(cap, "symref=HEAD:refs/heads/")
					}
				}
				line = line[:i]
			}
		}
		sha, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if ref == "HEAD" {
			headSHA = sha
			continue
		}
		if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok && len(res.Branches) < probeMaxBranches {
			res.Branches = append(res.Branches, b)
			shas[b] = sha
		}
	}
	if res.DefaultBranch == "" && len(res.Branches) > 0 {
		for _, b := range []string{"main", "master"} {
			for _, x := range res.Branches {
				if x == b {
					res.DefaultBranch = b
				}
			}
			if res.DefaultBranch != "" {
				break
			}
		}
		if res.DefaultBranch == "" {
			res.DefaultBranch = res.Branches[0]
		}
	}
	res.Head = shas[res.DefaultBranch]
	if res.Head == "" {
		res.Head = headSHA
	}
	return res, nil
}
