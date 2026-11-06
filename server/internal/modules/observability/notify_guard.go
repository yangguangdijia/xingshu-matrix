package observability

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// notifySecurityKey stores the outbound address policy for notification
// channels (a module-managed settings key, see settings.moduleManagedKey).
const notifySecurityKey = "notify.security"

// netPolicy guards every outbound connection a notification makes (webhooks,
// robots, custom SMTP) against SSRF. The check runs on the resolved IP at dial
// time, so DNS rebinding cannot bypass it; redirects are never followed.
type netPolicy struct {
	// BlockPrivate rejects loopback / private / link-local / CGNAT / ULA and
	// unspecified addresses. Default true.
	BlockPrivate bool `json:"block_private"`
	// AllowHosts are exceptions: "host", "host:port", an IP, or a CIDR
	// ("127.0.0.1:2525", "10.0.0.0/8", "hooks.internal"). Listed hosts may
	// also use plain http for robot channels (local receiving stubs).
	AllowHosts []string `json:"allow_hosts"`
}

func defaultNetPolicy() netPolicy { return netPolicy{BlockPrivate: true, AllowHosts: []string{}} }

// permissivePolicy is only used by the package-level sendChannel helper
// (unit tests); every module path goes through m.sender with the stored policy.
func permissivePolicy() netPolicy { return netPolicy{BlockPrivate: false} }

func (p *netPolicy) normalize() error {
	out := make([]string, 0, len(p.AllowHosts))
	seen := map[string]bool{}
	for _, h := range p.AllowHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || seen[h] {
			continue
		}
		if len(out) >= 64 {
			return httpx.Validation("白名单最多 64 项")
		}
		if strings.ContainsAny(h, " /\\?#@") && !strings.Contains(h, "/") {
			return httpx.Validation("白名单项格式不正确：" + h)
		}
		if strings.Contains(h, "/") {
			if _, _, err := net.ParseCIDR(h); err != nil {
				return httpx.Validation("CIDR 格式不正确：" + h)
			}
		} else if host, port, err := net.SplitHostPort(h); err == nil {
			if host == "" {
				return httpx.Validation("白名单项缺少主机：" + h)
			}
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return httpx.Validation("白名单端口不正确：" + h)
			}
		}
		seen[h] = true
		out = append(out, h)
	}
	p.AllowHosts = out
	return nil
}

// hostListed reports whether host (and optional port) is explicitly allowed by
// name or IP (CIDR entries are checked against resolved IPs in ipListed).
func (p netPolicy) hostListed(host, port string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	for _, a := range p.AllowHosts {
		if strings.Contains(a, "/") {
			if ip := net.ParseIP(host); ip != nil {
				if _, n, err := net.ParseCIDR(a); err == nil && n.Contains(ip) {
					return true
				}
			}
			continue
		}
		if h, pt, err := net.SplitHostPort(a); err == nil {
			if strings.Trim(h, "[]") == host && pt == port {
				return true
			}
			continue
		}
		if strings.Trim(a, "[]") == host {
			return true
		}
	}
	return false
}

func (p netPolicy) ipListed(ip net.IP, port string) bool {
	return p.hostListed(ip.String(), port)
}

var metadataIP = net.ParseIP("169.254.169.254")

// isInternalIP covers the address ranges an SSRF must never reach by default.
func isInternalIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 carrier-grade NAT, 0.0.0.0/8, 192.0.0.0/24, 198.18.0.0/15 benchmarking
		return v4[0] == 0 || (v4[0] == 100 && v4[1]&0xc0 == 64) || (v4[0] == 192 && v4[1] == 0 && v4[2] == 0) ||
			(v4[0] == 198 && (v4[1] == 18 || v4[1] == 19))
	}
	return false
}

// checkIP decides whether a resolved address may be dialed.
func (p netPolicy) checkIP(ip net.IP, port string) error {
	if p.ipListed(ip, port) {
		return nil
	}
	if ip.Equal(metadataIP) {
		return fmt.Errorf("目标地址 %s 为云元数据地址，禁止访问", ip)
	}
	if p.BlockPrivate && isInternalIP(ip) {
		return fmt.Errorf("目标地址 %s 属于内网 / 本机地址，已被通知安全策略拦截（可在「通知安全」中加入白名单）", ip)
	}
	return nil
}

// dialContext resolves the host, verifies every candidate address and dials
// the verified IP directly (no second lookup → no DNS rebinding window).
func (p netPolicy) dialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if p.hostListed(host, port) {
			return d.DialContext(ctx, network, addr)
		}
		var ips []net.IP
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
			ips = []net.IP{ip}
		} else {
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
			cancel()
			if err != nil {
				return nil, err
			}
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("无法解析 %s", host)
		}
		var lastErr error
		for _, ip := range ips {
			if err := p.checkIP(ip, port); err != nil {
				lastErr = err
				continue
			}
			c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// httpClient returns a client that enforces the policy and never follows redirects.
func (p netPolicy) httpClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil, // an environment proxy would bypass the IP check
		DialContext:           p.dialContext(d),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// checkURL is the save-time validation of a channel address. It rejects IP
// literals and names that currently resolve to blocked ranges; the dial-time
// check above remains authoritative.
func (p netPolicy) checkURL(ctx context.Context, raw string, requireHTTPS bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return httpx.Validation("地址必须是完整的 http(s) URL")
	}
	if u.User != nil {
		return httpx.Validation("地址中不能包含账号密码")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	listed := p.hostListed(u.Hostname(), port)
	if requireHTTPS && u.Scheme != "https" && !listed {
		return httpx.Validation("机器人地址必须使用 https（http 仅允许白名单内的收件桩）")
	}
	return p.checkHost(ctx, u.Hostname(), port)
}

func (p netPolicy) checkHost(ctx context.Context, host, port string) error {
	if p.hostListed(host, port) {
		return nil
	}
	var ips []net.IP
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		ips = []net.IP{ip}
	} else if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		ips = []net.IP{net.IPv4(127, 0, 0, 1)}
	} else {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
		cancel()
		if err != nil {
			return nil // unresolvable now (e.g. split DNS); enforced again at send time
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if err := p.checkIP(ip, port); err != nil {
			return httpx.Validation(err.Error())
		}
	}
	return nil
}

func (m *Module) netPolicy(ctx context.Context) netPolicy {
	p := defaultNetPolicy()
	if m.d != nil && m.d.Settings != nil {
		var stored netPolicy
		if err := m.d.Settings.Get(ctx, notifySecurityKey, &stored); err == nil {
			p = stored
			_ = p.normalize()
		}
	}
	if p.AllowHosts == nil {
		p.AllowHosts = []string{}
	}
	return p
}

func (m *Module) getNotifySecurity(c *gin.Context) { httpx.OK(c, m.netPolicy(c)) }

func (m *Module) putNotifySecurity(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	all := p != nil && p.EnvScope == nil
	if p != nil {
		for _, env := range p.EnvScope {
			all = all || env == "*"
		}
	}
	if !all {
		httpx.Fail(c, httpx.Forbidden("修改通知安全策略需要不受限的环境范围"))
		return
	}
	var in netPolicy
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.normalize(); err != nil {
		httpx.Fail(c, err)
		return
	}
	raw, _ := jsonMarshal(in)
	if m.d.Settings == nil {
		httpx.Fail(c, httpx.Internal(errors.New("settings unavailable")))
		return
	}
	if _, err := m.d.Settings.Put(c, notifySecurityKey, raw, false, "通知渠道出站地址策略", p.UserID); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "setting", notifySecurityKey)
	httpx.OK(c, in)
}
