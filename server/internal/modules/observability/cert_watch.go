package observability

// 证书到期提醒（v1.29 g-cert）。
//
// 定时（默认每 6 小时）扫描所有已登记集群中 type=kubernetes.io/tls 的 Secret
// 以及用户配置的域名（TLS 握手读取对端证书），解析叶子证书 NotAfter，
// 写入 obs_cert_status 快照；按阈值（默认 30/14/7/1 天，可配置）通过通知中心
// 发送 certificate 事件。每张证书（以 NotAfter 区分，续期后重新计数）每个阈值
// 只提醒一次，去重记录在 0008 的 obs_cert_notices。
//
// 只读取 tls.crt；tls.key 不解析、不落库、不出现在接口与日志里。

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	certSourceSecret = "secret"
	certSourceDomain = "domain"

	certDefaultIntervalHours = 6
	certTickInterval         = time.Minute
	certScanTimeout          = 5 * time.Minute
	certManualScanTimeout    = 2 * time.Minute
	certClusterTimeout       = 45 * time.Second
	certProbeTimeout         = 8 * time.Second
	certProbeConcurrency     = 8
	certMaxSecretsPerCluster = 5000
	certMaxDomains           = 500
)

var certDefaultThresholds = []int{30, 14, 7, 1}

// certScanMu keeps one scan per process; replicas coordinate through the
// last_scan_at compare-and-set in claimScheduledCertScan.
var certScanMu sync.Mutex

// 通知中心：注册 certificate 事件类型与默认模板（0008 已放开 event_kind 约束）。
func init() {
	if !validKind("certificate") {
		out := make([]string, 0, len(eventKinds)+1)
		for _, k := range eventKinds {
			if k == "test" {
				out = append(out, "certificate")
			}
			out = append(out, k)
		}
		if len(out) == len(eventKinds) {
			out = append(out, "certificate")
		}
		eventKinds = out
	}
	eventStatuses["certificate"] = []string{"expiring", "expired"}
	kindText["certificate"] = "证书"
	statusText["expiring"] = "即将到期"
	statusText["expired"] = "已过期"
	builtinTemplates["certificate"] = builtinTemplate{EventKind: "certificate", Name: "默认证书到期模板",
		TitleTpl: `【证书{{.StatusText}}】{{.AlertName}}{{if label "days_left" .Labels}}（剩余 {{label "days_left" .Labels}} 天）{{end}}`,
		BodyTpl: `证书：{{.AlertName}}
状态：{{.StatusText}}
{{if .Env}}环境：{{.EnvText}}
{{end}}{{if .Cluster}}集群：{{.Cluster}}
{{end}}{{if .Namespace}}命名空间：{{.Namespace}}
{{end}}{{if .Resource}}资源：{{.Resource}}
{{end}}{{if label "not_after" .Labels}}到期时间：{{label "not_after" .Labels}}
{{end}}{{if label "dns_names" .Labels}}域名：{{label "dns_names" .Labels}}
{{end}}{{if .Summary}}说明：{{.Summary}}
{{end}}{{if .Link}}查看：{{.Link}}{{end}}`}
}

// ---------------------------------------------------------------- pure logic

// certInfo is what a scan learns about one certificate (or a failed probe).
type certInfo struct {
	Source      string     `json:"source"`
	ClusterID   int64      `json:"cluster_id"`
	ClusterName string     `json:"cluster_name"`
	Env         string     `json:"env"`
	Namespace   string     `json:"namespace"`
	Name        string     `json:"name"` // Secret 名称，或域名 host:port
	Subject     string     `json:"subject"`
	Issuer      string     `json:"issuer"`
	DNSNames    string     `json:"dns_names"`
	NotAfter    *time.Time `json:"not_after"`
	Error       string     `json:"error,omitempty"`
	ScannedAt   time.Time  `json:"scanned_at"`
	DaysLeft    *int       `json:"days_left"`
	Status      string     `json:"status"` // ok | warning | critical | expired | error
}

// parseCertPEM returns the first (leaf) certificate of a PEM bundle.
func parseCertPEM(data []byte) (*x509.Certificate, error) {
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("未找到 PEM 格式证书")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("证书解析失败")
		}
		return c, nil
	}
}

func fillFromCert(ci *certInfo, c *x509.Certificate) {
	na := c.NotAfter.UTC()
	ci.NotAfter = &na
	ci.Subject = firstN(c.Subject.String(), 256)
	ci.Issuer = firstN(c.Issuer.String(), 256)
	names := c.DNSNames
	if len(names) > 20 {
		names = names[:20]
	}
	ci.DNSNames = firstN(strings.Join(names, ","), 1024)
}

// certDaysLeft is whole days until notAfter (negative once expired).
func certDaysLeft(notAfter, now time.Time) int {
	return int(math.Floor(notAfter.Sub(now).Hours() / 24))
}

// certStatusFor classifies remaining days against the largest threshold.
func certStatusFor(days int, thresholds []int) string {
	switch {
	case days < 0:
		return "expired"
	case days <= 7:
		return "critical"
	case len(thresholds) > 0 && days <= thresholds[0]:
		return "warning"
	default:
		return "ok"
	}
}

// normalizeThresholds dedups, validates (1..365) and sorts thresholds descending.
func normalizeThresholds(in []int) ([]int, error) {
	if len(in) == 0 || len(in) > 10 {
		return nil, httpx.Validation("提醒阈值需为 1 到 10 个")
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, t := range in {
		if t < 1 || t > 365 {
			return nil, httpx.Validation("提醒阈值需在 1 到 365 天之间")
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out, nil
}

// noticeThresholds returns the tightest threshold the certificate has crossed
// (0 means expired) and every threshold it has crossed. ok=false: nothing due.
func noticeThresholds(days int, thresholds []int) (target int, crossed []int, ok bool) {
	target = math.MaxInt
	for _, t := range thresholds {
		if days <= t {
			crossed = append(crossed, t)
			if t < target {
				target = t
			}
		}
	}
	if days < 0 {
		crossed = append(crossed, 0)
		target = 0
	}
	if len(crossed) == 0 {
		return 0, nil, false
	}
	return target, crossed, true
}

// certClaimer records that a notice was sent. Claim returns true only for the
// caller that first claims target for this certificate (and NotAfter); the
// other crossed thresholds are marked too so looser ones never fire later.
type certClaimer interface {
	Claim(ctx context.Context, ci *certInfo, target int, crossed []int) (bool, error)
}

type certNotice struct {
	Cert      *certInfo
	Threshold int
	DaysLeft  int
}

// decideCertNotices applies thresholds and dedup to a scan result.
func decideCertNotices(ctx context.Context, certs []*certInfo, thresholds []int, now time.Time, cl certClaimer) ([]certNotice, error) {
	var out []certNotice
	var firstErr error
	for _, ci := range certs {
		if ci.NotAfter == nil || ci.Error != "" {
			continue
		}
		days := certDaysLeft(*ci.NotAfter, now)
		target, crossed, ok := noticeThresholds(days, thresholds)
		if !ok {
			continue
		}
		claimed, err := cl.Claim(ctx, ci, target, crossed)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if claimed {
			out = append(out, certNotice{Cert: ci, Threshold: target, DaysLeft: days})
		}
	}
	return out, firstErr
}

func certNotifyEvent(n certNotice, link string, now time.Time) *NotifyEvent {
	ci := n.Cert
	na := ci.NotAfter.In(notifyLoc()).Format("2006-01-02 15:04")
	ev := &NotifyEvent{Kind: "certificate", Status: "expiring", Severity: "warning", Env: ci.Env, Cluster: ci.ClusterName,
		Namespace: ci.Namespace, StartsAtT: now, Link: link,
		Labels: map[string]string{"source": ci.Source, "days_left": strconv.Itoa(n.DaysLeft), "not_after": na,
			"threshold": strconv.Itoa(n.Threshold)}}
	if ci.DNSNames != "" {
		ev.Labels["dns_names"] = firstN(ci.DNSNames, 200)
	}
	if ci.Source == certSourceDomain {
		ev.AlertName, ev.Resource = ci.Name, "域名 "+ci.Name
	} else {
		ev.AlertName, ev.Resource = ci.Namespace+"/"+ci.Name, "Secret "+ci.Name
	}
	if n.DaysLeft <= 7 {
		ev.Severity = "critical"
	}
	if n.Threshold == 0 {
		ev.Status, ev.Severity = "expired", "critical"
		ev.Summary = "证书已于 " + na + " 过期，请尽快续期"
	} else {
		ev.Summary = fmt.Sprintf("证书将在 %d 天后（%s）到期，已进入 %d 天提醒阈值", n.DaysLeft, na, n.Threshold)
	}
	ev.Key = fmt.Sprintf("certificate:%s:%d:%s/%s:%d:%d", ci.Source, ci.ClusterID, ci.Namespace, ci.Name, ci.NotAfter.Unix(), n.Threshold)
	return ev
}

// ---------------------------------------------------------------- domains

type certDomain struct {
	ID        int64     `json:"id"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Env       string    `json:"env"`
	Remark    string    `json:"remark"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

func (d *certDomain) target() string { return net.JoinHostPort(d.Host, strconv.Itoa(d.Port)) }

type certDomainInput struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Env    string `json:"env"`
	Remark string `json:"remark"`
}

// validate normalizes host (lowercase, strips scheme/path, accepts host:port).
func (in *certDomainInput) validate() error {
	h := strings.TrimSpace(strings.ToLower(in.Host))
	for _, p := range []string{"https://", "http://"} {
		h = strings.TrimPrefix(h, p)
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if host, port, err := net.SplitHostPort(h); err == nil {
		p, perr := strconv.Atoi(port)
		if perr != nil {
			return httpx.Validation("端口格式不正确")
		}
		if in.Port != 0 && in.Port != p {
			return httpx.Validation("域名中的端口与端口字段不一致")
		}
		h, in.Port = host, p
	}
	h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
	if h == "" {
		return httpx.Validation("请填写域名")
	}
	if net.ParseIP(h) == nil && !validDNSName(h) {
		return httpx.Validation("域名格式不正确")
	}
	if in.Port == 0 {
		in.Port = 443
	}
	if in.Port < 1 || in.Port > 65535 {
		return httpx.Validation("端口需在 1 到 65535 之间")
	}
	in.Host = h
	in.Env = strings.TrimSpace(in.Env)
	if len(in.Env) > 32 {
		return httpx.Validation("环境标识过长")
	}
	in.Remark = strings.TrimSpace(in.Remark)
	if len([]rune(in.Remark)) > 128 {
		return httpx.Validation("备注不能超过 128 个字符")
	}
	return nil
}

func validDNSName(h string) bool {
	if len(h) > 253 {
		return false
	}
	labels := strings.Split(h, ".")
	for i, l := range labels {
		if l == "" || len(l) > 63 {
			return false
		}
		if i == 0 && l == "*" {
			return false // 通配符无法探测
		}
		for j, r := range l {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && j > 0 && j < len(l)-1) || r == '_'
			if !ok {
				return false
			}
		}
	}
	return true
}

// probeDomainCert dials host:port and reads the leaf certificate. Verification
// is skipped on purpose: expired or self-signed certificates must still be read.
// The connection is closed right after the handshake; no application data is sent.
func probeDomainCert(ctx context.Context, host string, port int) (*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(ctx, certProbeTimeout)
	defer cancel()
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: certProbeTimeout},
		Config: &tls.Config{ServerName: sniName(host), InsecureSkipVerify: true, MinVersion: tls.VersionTLS10}} //nolint:gosec // 只读取证书到期时间
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, errors.New("连接超时")
		}
		if strings.Contains(err.Error(), "no such host") {
			return nil, errors.New("域名无法解析")
		}
		if strings.Contains(err.Error(), "refused") {
			return nil, errors.New("连接被拒绝")
		}
		return nil, errors.New("TLS 握手失败")
	}
	defer conn.Close()
	st := conn.(*tls.Conn).ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return nil, errors.New("对端未返回证书")
	}
	return st.PeerCertificates[0], nil
}

func sniName(host string) string {
	if net.ParseIP(host) != nil {
		return ""
	}
	return host
}

// ---------------------------------------------------------------- config

type certWatchConfig struct {
	Enabled       bool            `json:"enabled"`
	IntervalHours int             `json:"interval_hours"`
	Thresholds    []int           `json:"thresholds"`
	LastScanAt    *time.Time      `json:"last_scan_at"`
	LastResult    json.RawMessage `json:"last_result"`
	UpdatedBy     string          `json:"updated_by"`
	UpdatedAt     *time.Time      `json:"updated_at"`
}

type certWatchConfigInput struct {
	Enabled       *bool `json:"enabled"`
	IntervalHours int   `json:"interval_hours"`
	Thresholds    []int `json:"thresholds"`
}

func (in *certWatchConfigInput) validate() error {
	if in.IntervalHours < 1 || in.IntervalHours > 168 {
		return httpx.Validation("扫描周期需在 1 到 168 小时之间")
	}
	t, err := normalizeThresholds(in.Thresholds)
	if err != nil {
		return err
	}
	in.Thresholds = t
	return nil
}

func (m *Module) loadCertConfig(ctx context.Context) (certWatchConfig, error) {
	cfg := certWatchConfig{Enabled: true, IntervalHours: certDefaultIntervalHours, Thresholds: append([]int(nil), certDefaultThresholds...)}
	var th []int32
	var res []byte
	var updAt time.Time
	err := m.d.DB.QueryRow(ctx, `SELECT enabled,interval_hours,thresholds,last_scan_at,last_result,updated_by,updated_at FROM obs_cert_watch_config WHERE id=1`).
		Scan(&cfg.Enabled, &cfg.IntervalHours, &th, &cfg.LastScanAt, &res, &cfg.UpdatedBy, &updAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	cfg.UpdatedAt = &updAt
	cfg.LastResult = res
	ints := make([]int, 0, len(th))
	for _, t := range th {
		ints = append(ints, int(t))
	}
	if n, err := normalizeThresholds(ints); err == nil {
		cfg.Thresholds = n
	}
	return cfg, nil
}

// ---------------------------------------------------------------- scanning

type certScanResult struct {
	Trigger       string            `json:"trigger"` // schedule | manual
	StartedAt     time.Time         `json:"started_at"`
	DurationMS    int64             `json:"duration_ms"`
	Clusters      int               `json:"clusters"`
	ClusterErrors map[string]string `json:"cluster_errors,omitempty"`
	Secrets       int               `json:"secrets"`
	Domains       int               `json:"domains"`
	DomainErrors  int               `json:"domain_errors"`
	Notices       int               `json:"notices"`
	NoticeError   string            `json:"notice_error,omitempty"`
}

type dbCertClaimer struct{ m *Module }

func (c dbCertClaimer) Claim(ctx context.Context, ci *certInfo, target int, crossed []int) (bool, error) {
	tx, err := c.m.d.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO obs_cert_notices (cluster_id,source,namespace,name,not_after,threshold)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, ci.ClusterID, ci.Source, ci.Namespace, ci.Name, *ci.NotAfter, target)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	for _, t := range crossed {
		if t == target {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO obs_cert_notices (cluster_id,source,namespace,name,not_after,threshold)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, ci.ClusterID, ci.Source, ci.Namespace, ci.Name, *ci.NotAfter, t); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// startCertWatch runs the periodic scanner until ctx is cancelled.
func (m *Module) startCertWatch(ctx context.Context) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		tick := time.NewTicker(certTickInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if !m.claimScheduledCertScan(ctx) {
				continue
			}
			if _, err := m.runCertScan(ctx, "schedule"); err != nil && !errors.Is(err, errCertScanBusy) {
				m.logWarn("certificate scan", err)
			}
		}
	}()
}

// claimScheduledCertScan advances last_scan_at with compare-and-set, so only
// one replica runs each scheduled scan.
func (m *Module) claimScheduledCertScan(ctx context.Context) bool {
	if m.d == nil || m.d.DB == nil {
		return false
	}
	var one int
	err := m.d.DB.QueryRow(ctx, `UPDATE obs_cert_watch_config SET last_scan_at=now()
		WHERE id=1 AND enabled AND (last_scan_at IS NULL OR last_scan_at < now() - make_interval(hours => interval_hours))
		RETURNING 1`).Scan(&one)
	return err == nil
}

var errCertScanBusy = errors.New("certificate scan already running")

func (m *Module) runCertScan(ctx context.Context, trigger string) (*certScanResult, error) {
	if !certScanMu.TryLock() {
		return nil, errCertScanBusy
	}
	defer certScanMu.Unlock()
	timeout := certScanTimeout
	if trigger == "manual" {
		timeout = certManualScanTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := &certScanResult{Trigger: trigger, StartedAt: time.Now()}
	cfg, err := m.loadCertConfig(ctx)
	if err != nil {
		return nil, err
	}
	var all []*certInfo

	// 1. 集群内 TLS Secret
	rows, err := m.d.DB.Query(ctx, `SELECT id,name,env,api_server FROM cluster_clusters ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var clusters []healthCluster
	for rows.Next() {
		var cl healthCluster
		if rows.Scan(&cl.ID, &cl.Name, &cl.Env, &cl.API) == nil {
			clusters = append(clusters, cl)
		}
	}
	rows.Close()
	res.Clusters = len(clusters)
	for _, cl := range clusters {
		if ctx.Err() != nil {
			break
		}
		certs, err := m.scanClusterSecrets(ctx, cl)
		if err != nil {
			if res.ClusterErrors == nil {
				res.ClusterErrors = map[string]string{}
			}
			res.ClusterErrors[cl.Name] = err.Error()
			continue // 保留该集群上次快照
		}
		res.Secrets += len(certs)
		if err := m.saveCertSnapshot(ctx, certSourceSecret, cl.ID, certs, res.StartedAt); err != nil {
			return nil, err
		}
		all = append(all, certs...)
	}
	if _, err := m.d.DB.Exec(ctx, `DELETE FROM obs_cert_status s WHERE source='secret'
		AND NOT EXISTS (SELECT 1 FROM cluster_clusters c WHERE c.id=s.cluster_id)`); err != nil {
		m.logWarn("certificate snapshot cleanup", err)
	}

	// 2. 域名探测
	domains, err := m.listCertDomains(ctx)
	if err != nil {
		return nil, err
	}
	res.Domains = len(domains)
	dc := m.probeDomains(ctx, domains)
	for _, ci := range dc {
		if ci.Error != "" {
			res.DomainErrors++
		}
	}
	if err := m.saveCertSnapshot(ctx, certSourceDomain, 0, dc, res.StartedAt); err != nil {
		return nil, err
	}
	all = append(all, dc...)

	// 3. 阈值提醒
	now := time.Now()
	notices, nerr := decideCertNotices(ctx, all, cfg.Thresholds, now, dbCertClaimer{m})
	if nerr != nil {
		res.NoticeError = "部分提醒记录写入失败"
		m.logWarn("certificate notice claim", nerr)
	}
	if len(notices) > 0 {
		link := strings.TrimRight(m.aiopsPolicy(ctx).PublicURL, "/")
		if link != "" {
			link += "/#/observe/certificates"
		}
		evs := make([]*NotifyEvent, 0, len(notices))
		for _, n := range notices {
			evs = append(evs, certNotifyEvent(n, link, now))
		}
		m.dispatch(ctx, evs)
	}
	res.Notices = len(notices)
	res.DurationMS = time.Since(res.StartedAt).Milliseconds()
	b, _ := json.Marshal(res)
	if _, err := m.d.DB.Exec(context.WithoutCancel(ctx), `UPDATE obs_cert_watch_config SET last_scan_at=$1,last_result=$2 WHERE id=1`, res.StartedAt, b); err != nil {
		m.logWarn("certificate scan result", err)
	}
	return res, nil
}

func (m *Module) scanClusterSecrets(ctx context.Context, cl healthCluster) ([]*certInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, certClusterTimeout)
	defer cancel()
	cs, err := m.healthClient(ctx, cl)
	if err != nil {
		return nil, err
	}
	var out []*certInfo
	opts := metav1.ListOptions{FieldSelector: "type=kubernetes.io/tls", Limit: 200}
	for {
		list, err := cs.CoreV1().Secrets("").List(ctx, opts)
		if err != nil {
			return nil, errors.New("读取 TLS Secret 失败，请检查集群连接与 secrets 读取权限")
		}
		for i := range list.Items {
			s := &list.Items[i]
			ci := &certInfo{Source: certSourceSecret, ClusterID: cl.ID, ClusterName: cl.Name, Env: cl.Env,
				Namespace: s.Namespace, Name: s.Name}
			if c, err := parseCertPEM(s.Data["tls.crt"]); err != nil {
				ci.Error = err.Error()
			} else {
				fillFromCert(ci, c)
			}
			out = append(out, ci)
			if len(out) >= certMaxSecretsPerCluster {
				return out, nil
			}
		}
		if list.Continue == "" {
			return out, nil
		}
		opts.Continue = list.Continue
	}
}

func (m *Module) probeDomains(ctx context.Context, domains []certDomain) []*certInfo {
	out := make([]*certInfo, len(domains))
	sem := make(chan struct{}, certProbeConcurrency)
	var wg sync.WaitGroup
	for i := range domains {
		d := domains[i]
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			ci := &certInfo{Source: certSourceDomain, Env: d.Env, Name: d.target()}
			if c, err := probeDomainCert(ctx, d.Host, d.Port); err != nil {
				ci.Error = err.Error()
			} else {
				fillFromCert(ci, c)
			}
			out[i] = ci
		}(i)
	}
	wg.Wait()
	return out
}

// saveCertSnapshot upserts certs and drops rows of that scope not seen in this scan.
func (m *Module) saveCertSnapshot(ctx context.Context, source string, clusterID int64, certs []*certInfo, at time.Time) error {
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, ci := range certs {
		if _, err := tx.Exec(ctx, `INSERT INTO obs_cert_status (source,cluster_id,namespace,name,cluster_name,env,subject,issuer,dns_names,not_after,error,scanned_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (source,cluster_id,namespace,name) DO UPDATE SET cluster_name=EXCLUDED.cluster_name,env=EXCLUDED.env,
			subject=EXCLUDED.subject,issuer=EXCLUDED.issuer,dns_names=EXCLUDED.dns_names,not_after=EXCLUDED.not_after,
			error=EXCLUDED.error,scanned_at=EXCLUDED.scanned_at`,
			ci.Source, ci.ClusterID, ci.Namespace, ci.Name, ci.ClusterName, ci.Env, ci.Subject, ci.Issuer, ci.DNSNames, ci.NotAfter, ci.Error, at); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM obs_cert_status WHERE source=$1 AND cluster_id=$2 AND scanned_at < $3`, source, clusterID, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (m *Module) listCertDomains(ctx context.Context) ([]certDomain, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT id,host,port,env,remark,created_by,created_at FROM obs_cert_domains ORDER BY host,port LIMIT $1`, certMaxDomains)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []certDomain{}
	for rows.Next() {
		var d certDomain
		if err := rows.Scan(&d.ID, &d.Host, &d.Port, &d.Env, &d.Remark, &d.CreatedBy, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- handlers

// registerCertWatch mounts the certificate endpoints under /observability.
func (m *Module) registerCertWatch(g *gin.RouterGroup) {
	cg := g.Group("/certificates")
	cg.GET("", rbac.RequirePerm(PermAlertRead), m.listCertificates)
	cg.POST("/scan", rbac.RequirePerm(PermNotify), m.scanCertificates)
	cg.GET("/settings", rbac.RequirePerm(PermAlertRead), m.getCertSettings)
	cg.PUT("/settings", rbac.RequirePerm(PermNotify), m.putCertSettings)
	cg.GET("/domains", rbac.RequirePerm(PermAlertRead), m.listCertDomainsHandler)
	cg.POST("/domains", rbac.RequirePerm(PermNotify), m.createCertDomain)
	cg.DELETE("/domains/:id", rbac.RequirePerm(PermNotify), m.deleteCertDomain)
}

// canSeeCert: env-less domains are visible to everyone with read permission.
func canSeeCert(p *rbac.Principal, env string) bool { return env == "" || p.CanEnv(env) }

func (m *Module) listCertificates(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	cfg, err := m.loadCertConfig(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	source, status := c.Query("source"), c.Query("status")
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	rows, err := m.d.DB.Query(c, `SELECT source,cluster_id,namespace,name,cluster_name,env,subject,issuer,dns_names,not_after,error,scanned_at
		FROM obs_cert_status WHERE ($1='' OR source=$1) ORDER BY not_after ASC NULLS FIRST, cluster_name, namespace, name LIMIT 20000`, source)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	now := time.Now()
	items := []*certInfo{}
	counts := map[string]int{}
	for rows.Next() {
		ci := &certInfo{}
		if err := rows.Scan(&ci.Source, &ci.ClusterID, &ci.Namespace, &ci.Name, &ci.ClusterName, &ci.Env, &ci.Subject, &ci.Issuer,
			&ci.DNSNames, &ci.NotAfter, &ci.Error, &ci.ScannedAt); err != nil {
			httpx.Fail(c, err)
			return
		}
		if !canSeeCert(p, ci.Env) {
			continue
		}
		if ci.Error != "" || ci.NotAfter == nil {
			ci.Status = "error"
		} else {
			d := certDaysLeft(*ci.NotAfter, now)
			ci.DaysLeft = &d
			ci.Status = certStatusFor(d, cfg.Thresholds)
		}
		if q != "" && !strings.Contains(strings.ToLower(ci.Name+" "+ci.Namespace+" "+ci.ClusterName+" "+ci.DNSNames), q) {
			continue
		}
		counts[ci.Status]++
		if status != "" && ci.Status != status {
			continue
		}
		items = append(items, ci)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": items, "counts": counts, "settings": cfg})
}

func (m *Module) scanCertificates(c *gin.Context) {
	audit.SetResource(c, "obs_cert_watch", "scan")
	res, err := m.runCertScan(c.Request.Context(), "manual")
	if errors.Is(err, errCertScanBusy) {
		httpx.Fail(c, httpx.Conflict("已有证书扫描正在进行，请稍后再试"))
		return
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetDetail(c, gin.H{"clusters": res.Clusters, "secrets": res.Secrets, "domains": res.Domains, "notices": res.Notices})
	p := rbac.PrincipalFrom(c)
	if !unrestricted(p) {
		res.ClusterErrors = nil // 不向受限用户暴露其无权查看的集群
	}
	httpx.OK(c, res)
}

func (m *Module) getCertSettings(c *gin.Context) {
	cfg, err := m.loadCertConfig(c)
	respond(c, cfg, err)
}

func (m *Module) putCertSettings(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	if !unrestricted(p) {
		httpx.Fail(c, httpx.Forbidden("修改证书提醒设置需要全部环境权限"))
		return
	}
	var in certWatchConfigInput
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.validate(); err != nil {
		httpx.Fail(c, err)
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	th := make([]int32, len(in.Thresholds))
	for i, t := range in.Thresholds {
		th[i] = int32(t)
	}
	if _, err := m.d.DB.Exec(c, `INSERT INTO obs_cert_watch_config (id,enabled,interval_hours,thresholds,updated_by,updated_at)
		VALUES (1,$1,$2,$3,$4,now()) ON CONFLICT (id) DO UPDATE SET enabled=$1,interval_hours=$2,thresholds=$3,updated_by=$4,updated_at=now()`,
		enabled, in.IntervalHours, th, p.Username); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_cert_watch", "settings")
	audit.SetDetail(c, gin.H{"enabled": enabled, "interval_hours": in.IntervalHours, "thresholds": in.Thresholds})
	cfg, err := m.loadCertConfig(c)
	respond(c, cfg, err)
}

func (m *Module) listCertDomainsHandler(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	all, err := m.listCertDomains(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := make([]certDomain, 0, len(all))
	for _, d := range all {
		if canSeeCert(p, d.Env) {
			out = append(out, d)
		}
	}
	httpx.OK(c, out)
}

func (m *Module) createCertDomain(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	var in certDomainInput
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.validate(); err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.Env == "" && !unrestricted(p) {
		httpx.Fail(c, httpx.Forbidden("未指定环境的域名需要全部环境权限，请选择环境"))
		return
	}
	if in.Env != "" && !p.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("无权访问环境 "+in.Env))
		return
	}
	var n int
	if err := m.d.DB.QueryRow(c, `SELECT count(*) FROM obs_cert_domains`).Scan(&n); err != nil {
		httpx.Fail(c, err)
		return
	}
	if n >= certMaxDomains {
		httpx.Fail(c, httpx.Conflict(fmt.Sprintf("最多配置 %d 个域名", certMaxDomains)))
		return
	}
	d := certDomain{Host: in.Host, Port: in.Port, Env: in.Env, Remark: in.Remark, CreatedBy: p.Username}
	err := m.d.DB.QueryRow(c, `INSERT INTO obs_cert_domains (host,port,env,remark,created_by) VALUES ($1,$2,$3,$4,$5) RETURNING id,created_at`,
		d.Host, d.Port, d.Env, d.Remark, d.CreatedBy).Scan(&d.ID, &d.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			httpx.Fail(c, httpx.Conflict("该域名已在监测列表中"))
			return
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_cert_domain", strconv.FormatInt(d.ID, 10))
	audit.SetDetail(c, gin.H{"host": d.Host, "port": d.Port, "env": d.Env})
	// 立即探测一次，列表马上能看到结果；提醒仍由扫描统一发送。
	ci := m.probeDomains(c.Request.Context(), []certDomain{d})[0]
	if _, err := m.d.DB.Exec(c, `INSERT INTO obs_cert_status (source,cluster_id,namespace,name,env,subject,issuer,dns_names,not_after,error,scanned_at)
		VALUES ('domain',0,'',$1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (source,cluster_id,namespace,name) DO UPDATE SET env=EXCLUDED.env,subject=EXCLUDED.subject,issuer=EXCLUDED.issuer,
		dns_names=EXCLUDED.dns_names,not_after=EXCLUDED.not_after,error=EXCLUDED.error,scanned_at=EXCLUDED.scanned_at`,
		ci.Name, ci.Env, ci.Subject, ci.Issuer, ci.DNSNames, ci.NotAfter, ci.Error); err != nil {
		m.logWarn("certificate domain probe save", err)
	}
	httpx.Created(c, gin.H{"domain": d, "probe": ci})
}

func (m *Module) deleteCertDomain(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	p := rbac.PrincipalFrom(c)
	var d certDomain
	err := m.d.DB.QueryRow(c, `SELECT id,host,port,env FROM obs_cert_domains WHERE id=$1`, id).Scan(&d.ID, &d.Host, &d.Port, &d.Env)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("域名不存在"))
		return
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if (d.Env == "" && !unrestricted(p)) || (d.Env != "" && !p.CanEnv(d.Env)) {
		httpx.Fail(c, httpx.Forbidden("无权删除该域名"))
		return
	}
	tx, err := m.d.DB.Begin(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer func() { _ = tx.Rollback(c) }()
	if _, err := tx.Exec(c, `DELETE FROM obs_cert_domains WHERE id=$1`, id); err != nil {
		httpx.Fail(c, err)
		return
	}
	if _, err := tx.Exec(c, `DELETE FROM obs_cert_status WHERE source='domain' AND cluster_id=0 AND namespace='' AND name=$1`, d.target()); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := tx.Commit(c); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_cert_domain", strconv.FormatInt(id, 10))
	audit.SetDetail(c, gin.H{"host": d.Host, "port": d.Port})
	httpx.OK(c, gin.H{"deleted": id})
}
