package cicd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	helmRepoMaxIndex = 8 << 20
	helmRepoTimeout  = 20 * time.Second
)

type helmRepoIn struct {
	Name string `json:"name" binding:"required,max=64"`
	URL  string `json:"url" binding:"required,max=2048"`
	Env  string `json:"env" binding:"required,max=64"`
}

type helmRepo struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	URL            string     `json:"url"`
	Env            string     `json:"env"`
	Auth           string     `json:"auth"`
	LastSyncAt     *time.Time `json:"last_sync_at,omitempty"`
	LastSyncStatus string     `json:"last_sync_status"`
	LastSyncError  string     `json:"last_sync_error,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type helmChart struct {
	Name        string    `json:"name" yaml:"name"`
	Version     string    `json:"version" yaml:"version"`
	AppVersion  string    `json:"app_version" yaml:"appVersion"`
	Description string    `json:"description" yaml:"description"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

type helmIndex struct {
	Entries map[string][]helmChart `yaml:"entries"`
}

func validateHelmRepoInput(in *helmRepoIn) error {
	if in == nil {
		return httpx.Validation("invalid Helm repository configuration")
	}
	in.Name, in.Env = strings.TrimSpace(in.Name), strings.TrimSpace(in.Env)
	if !validArgoDisplay(in.Name, 64) || inlineSecretText(in.Name) {
		return httpx.Validation("Helm repository name is required (max 64)")
	}
	if !validArgoEnv(in.Env) {
		return httpx.Validation("env must use up to 64 letters, digits, dots, hyphens or underscores")
	}
	normalized, err := normalizeHelmRepoURL(in.URL)
	if err != nil {
		return err
	}
	in.URL = normalized
	return nil
}

func normalizeHelmRepoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return "", httpx.Validation("Helm repository URL is required (max 2048)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return "", httpx.Validation("invalid Helm repository URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" {
		return "", httpx.Validation("Helm repository URL must use HTTPS; OCI and HTTP repositories are unavailable")
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", httpx.Validation("Helm repository URL must not contain credentials, query, or fragment")
	}
	if strings.ContainsAny(u.Host, " \t\r\n") {
		return "", httpx.Validation("invalid Helm repository host")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return "", httpx.Validation("Helm repository URL must not contain dot segments")
		}
	}
	if err := validateHelmHost(u.Hostname()); err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func validateHelmHost(host string) error {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		h == "metadata" || h == "metadata.google.internal" || h == "metadata.azure.internal" {
		return errors.New("repository host is not allowed")
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if !helmIPAllowed(ip) {
			return errors.New("repository host resolves to a prohibited address")
		}
		return nil
	}
	if strings.Contains(h, ":") || strings.ContainsAny(h, " /\\") {
		return errors.New("invalid Helm repository host")
	}
	return nil
}

func helmIPAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// Cloud metadata endpoints are denied even though some are otherwise
	// routable/private and may be reached through an internal proxy.
	for _, raw := range []string{"169.254.169.254", "100.100.100.200"} {
		if ip == netip.MustParseAddr(raw) {
			return false
		}
	}
	if ip.Is6() && ip == netip.MustParseAddr("fd00:ec2::254") {
		return false
	}
	return true
}

type helmResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

func newHelmRepoHTTPClient(resolver helmResolver) *http.Client {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout: 30 * time.Second, MaxIdleConns: 4, MaxIdleConnsPerHost: 2,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("invalid repository address")
			}
			if err := validateHelmHost(host); err != nil {
				return nil, err
			}
			var ips []net.IPAddr
			if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
				ips = []net.IPAddr{{IP: net.IP(ip.AsSlice())}}
			} else {
				ips, err = resolver.LookupIPAddr(ctx, host)
				if err != nil || len(ips) == 0 {
					return nil, errors.New("repository host could not be resolved")
				}
			}
			for _, candidate := range ips {
				addr, ok := netip.AddrFromSlice(candidate.IP)
				if !ok || !helmIPAllowed(addr) {
					return nil, errors.New("repository host resolves to a prohibited address")
				}
			}
			var last error
			for _, candidate := range ips {
				target := net.JoinHostPort(candidate.IP.String(), port)
				conn, dialErr := dialer.DialContext(ctx, network, target)
				if dialErr == nil {
					return conn, nil
				}
				last = dialErr
			}
			return nil, fmt.Errorf("could not connect to repository: %w", last)
		},
	}
	return &http.Client{
		Transport: transport, Timeout: helmRepoTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("too many repository redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("repository redirects must remain HTTPS")
			}
			return validateHelmHost(req.URL.Hostname())
		},
	}
}

func decodeHelmIndex(raw []byte, now time.Time) ([]helmChart, error) {
	var index helmIndex
	if err := yaml.Unmarshal(raw, &index); err != nil {
		return nil, errors.New("Helm repository index is invalid YAML")
	}
	if len(index.Entries) > 10000 {
		return nil, errors.New("Helm repository index has too many charts")
	}
	charts := make([]helmChart, 0)
	for name, versions := range index.Entries {
		if !validArgoApp(name) || len(versions) > 1000 {
			continue
		}
		for _, chart := range versions {
			if chart.Version == "" || len(chart.Version) > 256 || strings.ContainsAny(chart.Version, "\r\n\x00") {
				continue
			}
			chart.Name = name
			chart.UpdatedAt = now
			charts = append(charts, chart)
			if len(charts) > 50000 {
				return nil, errors.New("Helm repository index has too many chart versions")
			}
		}
	}
	sort.Slice(charts, func(i, j int) bool {
		if charts[i].Name == charts[j].Name {
			return charts[i].Version > charts[j].Version
		}
		return charts[i].Name < charts[j].Name
	})
	return charts, nil
}

func fetchHelmIndex(ctx context.Context, client *http.Client, base string) ([]helmChart, error) {
	u, err := url.Parse(strings.TrimRight(base, "/") + "/index.yaml")
	if err != nil {
		return nil, errors.New("invalid Helm repository URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("could not create Helm index request")
	}
	req.Header.Set("Accept", "application/x-yaml, text/yaml, */*")
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Helm repository index request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Helm repository returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, helmRepoMaxIndex+1))
	if err != nil {
		return nil, errors.New("Helm repository index could not be read")
	}
	if len(body) > helmRepoMaxIndex {
		return nil, errors.New("Helm repository index exceeds 8 MiB")
	}
	return decodeHelmIndex(body, time.Now().UTC())
}

func (m *Module) listHelmRepos(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	rows, err := m.db.Query(c, `SELECT id,name,url,env,created_at,updated_at,last_sync_at,last_sync_status,last_sync_error
		FROM cicd_helm_repos ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []helmRepo{}
	for rows.Next() {
		var item helmRepo
		var createdAt time.Time
		if err := rows.Scan(&item.ID, &item.Name, &item.URL, &item.Env, &createdAt, &item.UpdatedAt,
			&item.LastSyncAt, &item.LastSyncStatus, &item.LastSyncError); err != nil {
			httpx.Fail(c, err)
			return
		}
		if !p.CanEnv(item.Env) {
			continue
		}
		item.Auth = "not_supported"
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, out)
}

func (m *Module) createHelmRepo(c *gin.Context) {
	var in helmRepoIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := validateHelmRepoInput(&in); err != nil {
		httpx.Fail(c, err)
		return
	}
	if !requireArgoScope(c, in.Env) {
		return
	}
	change := cicdConfigMutation{Kind: "helm.repo.create", HelmRepo: &in, Envs: []string{in.Env}}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	id, err := m.applyCICDConfigMutation(c, change)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("Helm repository name exists").Wrap(errors.New("configuration write failed")))
		return
	}
	audit.SetResource(c, "cicd_helm_repo", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"env": in.Env, "host": safeHelmHost(in.URL)})
	httpx.Created(c, gin.H{"id": id, "name": in.Name, "url": in.URL, "env": in.Env, "auth": "not_supported"})
}

func safeHelmHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	return u.Hostname()
}

func (m *Module) updateHelmRepo(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	current, updated, ok := m.helmRepoVersion(c, id)
	if !ok {
		return
	}
	var in helmRepoIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := validateHelmRepoInput(&in); err != nil {
		httpx.Fail(c, err)
		return
	}
	if !requireArgoScope(c, current.env) || !requireArgoScope(c, in.Env) {
		return
	}
	change := cicdConfigMutation{Kind: "helm.repo.update", ID: id, HelmRepo: &in,
		ExpectedUpdatedAt: &updated, Envs: []string{current.env, in.Env}}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	if _, err := m.applyCICDConfigMutation(c, change); err != nil {
		httpx.Fail(c, httpx.Conflict("Helm repository changed or name exists").Wrap(errors.New("configuration write failed")))
		return
	}
	audit.SetResource(c, "cicd_helm_repo", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"env": in.Env, "host": safeHelmHost(in.URL)})
	httpx.OK(c, gin.H{"id": id, "name": in.Name, "url": in.URL, "env": in.Env, "auth": "not_supported"})
}

type helmRepoVersion struct {
	name, url, env string
	lastSyncStatus string
	lastSyncError  string
	updated        time.Time
}

func (m *Module) helmRepoVersion(c *gin.Context, id int64) (helmRepoVersion, time.Time, bool) {
	var repo helmRepoVersion
	err := m.db.QueryRow(c, `SELECT name,url,env,last_sync_status,last_sync_error,updated_at FROM cicd_helm_repos WHERE id=$1`, id).
		Scan(&repo.name, &repo.url, &repo.env, &repo.lastSyncStatus, &repo.lastSyncError, &repo.updated)
	if err != nil {
		httpx.Fail(c, notFound(err))
		return helmRepoVersion{}, time.Time{}, false
	}
	if !requireArgoScope(c, repo.env) {
		return helmRepoVersion{}, time.Time{}, false
	}
	return repo, repo.updated, true
}

func (m *Module) deleteHelmRepo(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	repo, updated, ok := m.helmRepoVersion(c, id)
	if !ok {
		return
	}
	change := cicdConfigMutation{Kind: "helm.repo.delete", ID: id, ExpectedUpdatedAt: &updated, Envs: []string{repo.env}}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	if _, err := m.applyCICDConfigMutation(c, change); err != nil {
		httpx.Fail(c, httpx.Conflict("Helm repository is in use or changed").Wrap(errors.New("configuration delete failed")))
		return
	}
	audit.SetResource(c, "cicd_helm_repo", fmt.Sprint(id))
	httpx.OK(c, gin.H{"deleted": id})
}

func (m *Module) syncHelmRepoHTTP(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	repo, updated, ok := m.helmRepoVersion(c, id)
	if !ok {
		return
	}
	change := cicdConfigMutation{Kind: "helm.repo.sync", ID: id, ExpectedUpdatedAt: &updated, Envs: []string{repo.env}}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	if _, err := m.syncHelmRepo(c, id, &updated); err != nil {
		httpx.Fail(c, httpx.Conflict("Helm repository sync failed").Wrap(err))
		return
	}
	audit.SetResource(c, "cicd_helm_repo", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"env": repo.env, "host": safeHelmHost(repo.url)})
	httpx.OK(c, gin.H{"synced": true, "id": id})
}

func (m *Module) syncHelmRepo(ctx context.Context, id int64, expected *time.Time) (int64, error) {
	var rawURL string
	var updated time.Time
	if err := m.db.QueryRow(ctx, `SELECT url,updated_at FROM cicd_helm_repos WHERE id=$1`, id).
		Scan(&rawURL, &updated); err != nil {
		return 0, notFound(err)
	}
	if expected != nil && !updated.Equal(*expected) {
		return 0, httpx.Conflict("Helm repository changed while approval was pending")
	}
	base, err := normalizeHelmRepoURL(rawURL)
	if err != nil {
		return 0, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, helmRepoTimeout)
	defer cancel()
	charts, err := fetchHelmIndex(reqCtx, newHelmRepoHTTPClient(nil), base)
	if err != nil {
		_, _ = m.db.Exec(context.WithoutCancel(ctx), `UPDATE cicd_helm_repos SET last_sync_at=now(),last_sync_status='failed',
			last_sync_error=$2 WHERE id=$1`, id, safeHelmSyncError(err))
		return 0, err
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `UPDATE cicd_helm_repos SET last_sync_at=now(),last_sync_status='succeeded',last_sync_error='',updated_at=now()
		WHERE id=$1 AND updated_at=$2`, id, updated)
	if err != nil || tag.RowsAffected() == 0 {
		return 0, httpx.Conflict("Helm repository changed while synchronizing")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cicd_helm_charts WHERE repo_id=$1`, id); err != nil {
		return 0, err
	}
	for _, chart := range charts {
		if _, err := tx.Exec(ctx, `INSERT INTO cicd_helm_charts(repo_id,name,version,app_version,description,updated_at)
			VALUES($1,$2,$3,$4,$5,$6)`, id, chart.Name, chart.Version, chart.AppVersion, chart.Description, chart.UpdatedAt); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

func safeHelmSyncError(err error) string {
	if err == nil {
		return ""
	}
	message := redactCICDText(err.Error())
	if len(message) > 256 {
		message = message[:256]
	}
	return message
}

func (m *Module) listHelmCharts(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	query := strings.TrimSpace(c.Query("q"))
	if len(query) > 128 {
		httpx.Fail(c, httpx.Validation("search query is too long"))
		return
	}
	rows, err := m.db.Query(c, `SELECT r.id,r.name,r.env,c.name,c.version,c.app_version,c.description,c.updated_at
		FROM cicd_helm_charts c JOIN cicd_helm_repos r ON r.id=c.repo_id
		WHERE ($1::boolean OR r.env = ANY($2::text[])) AND ($3='' OR c.name ILIKE '%'||$3||'%' OR c.description ILIKE '%'||$3||'%')
		ORDER BY c.name,c.version DESC LIMIT 1000`, argoScopeAll(p.EnvScope), p.EnvScope, query)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var repoID int64
		var repoName, env string
		var chart helmChart
		if err := rows.Scan(&repoID, &repoName, &env, &chart.Name, &chart.Version, &chart.AppVersion, &chart.Description, &chart.UpdatedAt); err != nil {
			httpx.Fail(c, err)
			return
		}
		out = append(out, gin.H{"repo_id": repoID, "repo_name": repoName, "env": env, "chart": chart})
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": out, "total": len(out), "install_supported": false, "auth_supported": false, "oci_supported": false})
}
