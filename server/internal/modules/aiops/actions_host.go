package aiops

// Host actions (ai-host): run on bastion assets through Deps.RemoteExec, except
// host.http_check, which the platform performs itself. Every parameter that
// ends up in a shell command is validated against a strict whitelist regexp
// AND single-quoted, so a value can never break out of its argument.

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

func init() {
	registerAction(hostProbeAction{})
	registerAction(hostCheckPortAction{})
	registerAction(hostHTTPCheckAction{})
	registerAction(hostServiceAction{})
	registerAction(hostExecAction{})
	registerAction(hostDeployAction{})
}

// ---------------------------------------------------------------------------
// shared helpers

const (
	hostPermConnect   = "bastion:connect"
	hostMaxOutput     = 64 << 10
	hostMaxCommandLen = 4096
	hostMaxComposeLen = 256 << 10

	hostMarkPrevState     = "__AIOPS_PREV_STATE__="
	hostMarkPrevImage     = "__AIOPS_PREV_IMAGE__="
	hostMarkPrevContainer = "__AIOPS_PREV_CONTAINER__="
	hostMarkComposeBackup = "__AIOPS_COMPOSE_BACKUP__="
	hostMarkComposeFresh  = "__AIOPS_COMPOSE_FRESH__"
)

var (
	errHostExecDisabled = errors.New("堡垒机远程执行未启用")

	reHostService   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:\-]{0,127}$`)
	reHostName      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9\-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9\-]{0,62}))*$`)
	reHostImage     = regexp.MustCompile(`^[a-z0-9]([a-z0-9._\-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]([a-z0-9._\-]*[a-z0-9])?)*(:[A-Za-z0-9_][A-Za-z0-9_.\-]{0,127})?(@sha256:[a-f0-9]{64})?$`)
	reHostContainer = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,62}$`)
	reHostPortMap   = regexp.MustCompile(`^((\d{1,3}\.){3}\d{1,3}:)?\d{1,5}:\d{1,5}(/(tcp|udp))?$`)
	reHostVolume    = regexp.MustCompile(`^(/[A-Za-z0-9._/\-]*|[A-Za-z0-9][A-Za-z0-9_.\-]*):/[A-Za-z0-9._/\-]*(:(ro|rw))?$`)
	reHostEnvKey    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	reHostAbsPath   = regexp.MustCompile(`^/[A-Za-z0-9._/\-]{0,255}$`)
	reHostFileName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]{0,63}\.ya?ml$`)
)

// hostShellQuote single-quotes s for POSIX sh.
func hostShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func hostIsProd(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "prod", "production", "prd", "生产":
		return true
	}
	return false
}

// hostTarget is embedded in every bastion action's params.
type hostTarget struct {
	AssetID   int64 `json:"asset_id"`
	AccountID int64 `json:"account_id,omitempty"`
}

func (t hostTarget) check() error {
	if t.AssetID <= 0 {
		return errors.New("asset_id 必填且必须为正整数")
	}
	if t.AccountID < 0 {
		return errors.New("account_id 不合法")
	}
	return nil
}

func hostProvider(ec *ExecContext) app.RemoteExecProvider {
	if ec == nil || ec.Deps == nil {
		return nil
	}
	return ec.Deps.RemoteExec
}

// hostDecode strictly decodes params (unknown fields rejected).
func hostDecode(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(params)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("参数格式错误：%v", err)
	}
	return nil
}

func hostEncode(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// hostRisk applies the environment / asset escalation rules to a base risk.
func hostRisk(ctx context.Context, ec *ExecContext, assetID int64, base string) (string, error) {
	risk := base
	if base == RiskRead {
		// read-only actions are never escalated, but the asset must be usable.
		if _, err := hostResolve(ctx, ec, assetID); err != nil {
			return "", err
		}
		return risk, nil
	}
	if ec != nil && hostIsProd(ec.Env) {
		risk = MaxRisk(risk, RiskHigh)
	}
	ref, err := hostResolve(ctx, ec, assetID)
	if err != nil {
		return "", err
	}
	if ref != nil && hostIsProd(ref.Env) {
		risk = MaxRisk(risk, RiskHigh)
	}
	return risk, nil
}

// hostResolve checks the asset when the bastion is available (nil, nil otherwise,
// so plans can still be drafted and reviewed without it).
func hostResolve(ctx context.Context, ec *ExecContext, assetID int64) (*app.AssetRef, error) {
	p := hostProvider(ec)
	if p == nil || assetID <= 0 {
		return nil, nil
	}
	ref, err := p.ResolveAsset(ctx, ec.Principal, assetID)
	if err != nil {
		return nil, fmt.Errorf("资产 #%d 不可用：%v", assetID, err)
	}
	return &ref, nil
}

func hostReason(ec *ExecContext, action string) string {
	r := "aiops " + action
	if ec != nil && ec.Case != nil {
		r = fmt.Sprintf("aiops case #%d %s", ec.Case.ID, action)
	}
	if ec != nil && ec.Reason != "" {
		r += " " + ec.Reason
	}
	return r
}

// hostRun executes cmd on the asset. A non-zero exit code returns the output
// together with an error.
func hostRun(ctx context.Context, ec *ExecContext, t hostTarget, action, cmd string, timeout time.Duration) (string, error) {
	p := hostProvider(ec)
	if p == nil {
		return "", errHostExecDisabled
	}
	res, err := p.RemoteExec(ctx, ec.Principal, app.RemoteExecRequest{
		AssetID: t.AssetID, AccountID: t.AccountID, Command: cmd, Timeout: timeout, Reason: hostReason(ec, action),
	})
	if err != nil {
		return "", fmt.Errorf("堡垒机执行失败：%w", err)
	}
	out := hostBound(res.Output, res.Truncated)
	if res.SessionID != "" {
		out = fmt.Sprintf("[堡垒机会话 %s]\n%s", res.SessionID, out)
	}
	if res.Blocked != "" {
		return out, fmt.Errorf("命令被堡垒机命令过滤拦截：%s", res.Blocked)
	}
	if res.ExitCode != 0 {
		return out, fmt.Errorf("命令退出码 %d", res.ExitCode)
	}
	return out, nil
}

func hostBound(s string, truncated bool) string {
	s = ai.RedactSecrets(s)
	if len(s) > hostMaxOutput {
		s = s[:hostMaxOutput]
		truncated = true
	}
	if truncated {
		s += "\n...(输出已截断)"
	}
	return s
}

// hostMarker returns the value after the last line starting with mark.
func hostMarker(out, mark string) (string, bool) {
	val, ok := "", false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, mark) {
			val, ok = strings.TrimSpace(strings.TrimPrefix(line, mark)), true
		}
	}
	return val, ok
}

func hostManual(assetID int64, cmd string) string {
	return fmt.Sprintf("通过堡垒机登录资产 #%d 后执行：\n%s", assetID, ai.RedactSecrets(cmd))
}

func hostSchema(s string) json.RawMessage { return json.RawMessage(s) }

// ---------------------------------------------------------------------------
// host.probe

type hostProbeAction struct{}

const hostProbeCmd = `echo '== uname =='; uname -a; ` +
	`echo '== uptime =='; uptime; ` +
	`echo '== df =='; df -hP 2>/dev/null | head -n 40; ` +
	`echo '== free =='; free -m 2>/dev/null; ` +
	`echo '== top =='; top -b -n 1 2>/dev/null | head -n 20`

func (hostProbeAction) Name() string { return "host.probe" }
func (hostProbeAction) Spec() ActionSpec {
	return ActionSpec{
		Name: "host.probe", Title: "主机概况探测", Channel: "bastion", Risk: RiskRead, ReadOnly: true, Permission: hostPermConnect,
		Description: "经堡垒机只读采集主机概况：uname、uptime（负载）、df（磁盘）、free（内存）、top 前 20 行。",
		Params:      hostSchema(`{"type":"object","required":["asset_id"],"properties":{"asset_id":{"type":"integer","minimum":1},"account_id":{"type":"integer"}},"additionalProperties":false}`),
	}
}
func (a hostProbeAction) Validate(ctx context.Context, ec *ExecContext, params json.RawMessage) (json.RawMessage, string, error) {
	var p hostTarget
	if err := hostDecode(params, &p); err != nil {
		return nil, "", err
	}
	if err := p.check(); err != nil {
		return nil, "", err
	}
	risk, err := hostRisk(ctx, ec, p.AssetID, RiskRead)
	return hostEncode(p), risk, err
}
func (a hostProbeAction) Describe(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, string, string, error) {
	var p hostTarget
	if err := hostDecode(params, &p); err != nil {
		return "", "", "", err
	}
	return fmt.Sprintf("探测主机 #%d 概况", p.AssetID), hostProbeCmd, hostManual(p.AssetID, hostProbeCmd), nil
}
func (a hostProbeAction) Execute(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, error) {
	var p hostTarget
	if err := hostDecode(params, &p); err != nil {
		return "", err
	}
	if err := p.check(); err != nil {
		return "", err
	}
	return hostRun(ctx, ec, p, a.Name(), hostProbeCmd, 30*time.Second)
}
func (hostProbeAction) Rollback(context.Context, *ExecContext, json.RawMessage, string) (string, error) {
	return "", ErrNoRollback
}

// ---------------------------------------------------------------------------
// host.check_port

type hostCheckPortAction struct{}

type hostCheckPortParams struct {
	hostTarget
	Port   int    `json:"port"`
	Host   string `json:"host,omitempty"`   // address to connect to FROM the asset; default 127.0.0.1
	Expect string `json:"expect,omitempty"` // open | closed | "" (just report)
}

func (p *hostCheckPortParams) normalise() error {
	if err := p.check(); err != nil {
		return err
	}
	if p.Port < 1 || p.Port > 65535 {
		return errors.New("port 必须在 1-65535 之间")
	}
	if p.Host == "" {
		p.Host = "127.0.0.1"
	}
	if net.ParseIP(p.Host) == nil && !reHostName.MatchString(p.Host) {
		return fmt.Errorf("host 不合法：%q", p.Host)
	}
	switch p.Expect {
	case "", "open", "closed":
	default:
		return errors.New("expect 只能是 open 或 closed")
	}
	return nil
}

func (p hostCheckPortParams) command() string {
	port := strconv.Itoa(p.Port)
	probe := "exec 3<>/dev/tcp/" + p.Host + "/" + port
	return "echo '== listen =='; (ss -ltnup 2>/dev/null || netstat -ltnup 2>/dev/null) | grep -E " + hostShellQuote(":"+port+"([^0-9]|$)") + " || echo '(无监听记录)'; " +
		"if timeout 5 bash -c " + hostShellQuote(probe) + " 2>/dev/null; then echo PORT_OPEN; else echo PORT_CLOSED; fi"
}

func (hostCheckPortAction) Name() string { return "host.check_port" }
func (hostCheckPortAction) Spec() ActionSpec {
	return ActionSpec{
		Name: "host.check_port", Title: "端口检查", Channel: "bastion", Risk: RiskRead, ReadOnly: true, Permission: hostPermConnect,
		Description: "在目标主机上查看端口监听（ss/netstat）并尝试 TCP 连接 host:port（默认 127.0.0.1）。expect=open/closed 时结果不符视为失败，可作验证步骤。",
		Params:      hostSchema(`{"type":"object","required":["asset_id","port"],"properties":{"asset_id":{"type":"integer","minimum":1},"account_id":{"type":"integer"},"port":{"type":"integer","minimum":1,"maximum":65535},"host":{"type":"string","description":"从目标主机连接的地址，默认 127.0.0.1"},"expect":{"type":"string","enum":["open","closed"]}},"additionalProperties":false}`),
	}
}
func (a hostCheckPortAction) Validate(ctx context.Context, ec *ExecContext, params json.RawMessage) (json.RawMessage, string, error) {
	var p hostCheckPortParams
	if err := hostDecode(params, &p); err != nil {
		return nil, "", err
	}
	if err := p.normalise(); err != nil {
		return nil, "", err
	}
	risk, err := hostRisk(ctx, ec, p.AssetID, RiskRead)
	return hostEncode(p), risk, err
}
func (a hostCheckPortAction) Describe(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, string, string, error) {
	var p hostCheckPortParams
	if err := hostDecode(params, &p); err != nil {
		return "", "", "", err
	}
	if err := p.normalise(); err != nil {
		return "", "", "", err
	}
	cmd := p.command()
	return fmt.Sprintf("检查主机 #%d 端口 %s:%d", p.AssetID, p.Host, p.Port), cmd, hostManual(p.AssetID, cmd), nil
}
func (a hostCheckPortAction) Execute(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, error) {
	var p hostCheckPortParams
	if err := hostDecode(params, &p); err != nil {
		return "", err
	}
	if err := p.normalise(); err != nil {
		return "", err
	}
	out, err := hostRun(ctx, ec, p.hostTarget, a.Name(), p.command(), 20*time.Second)
	if err != nil {
		return out, err
	}
	open := strings.Contains(out, "PORT_OPEN")
	switch {
	case p.Expect == "open" && !open:
		return out, fmt.Errorf("端口 %s:%d 未开放", p.Host, p.Port)
	case p.Expect == "closed" && open:
		return out, fmt.Errorf("端口 %s:%d 仍在监听", p.Host, p.Port)
	}
	return out, nil
}
func (hostCheckPortAction) Rollback(context.Context, *ExecContext, json.RawMessage, string) (string, error) {
	return "", ErrNoRollback
}

// ---------------------------------------------------------------------------
// host.http_check (platform side, no bastion)

type hostHTTPCheckAction struct{}

type hostHTTPCheckParams struct {
	URL          string `json:"url"`
	Method       string `json:"method,omitempty"`        // GET | HEAD
	ExpectStatus []int  `json:"expect_status,omitempty"` // default: 200-399
	Contains     string `json:"contains,omitempty"`      // substring expected in the first 64 KiB
	TimeoutSec   int    `json:"timeout_sec,omitempty"`   // default 10, max 30
	Insecure     bool   `json:"insecure,omitempty"`      // skip TLS verification (self-signed internal endpoints)
}

// hostHTTPDo is replaceable in tests.
var hostHTTPDo = func(req *http.Request, insecure bool, timeout time.Duration) (*http.Response, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opted in for an internal endpoint
	}
	c := &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("重定向次数过多")
		}
		return nil
	}}
	return c.Do(req)
}

func (p *hostHTTPCheckParams) normalise() error {
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url 必须是 http:// 或 https:// 地址")
	}
	if u.User != nil {
		return errors.New("url 中不能包含账号密码")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && (ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()) {
		return errors.New("不允许访问链路本地/元数据地址")
	}
	p.URL = u.String()
	p.Method = strings.ToUpper(p.Method)
	if p.Method == "" {
		p.Method = http.MethodGet
	}
	if p.Method != http.MethodGet && p.Method != http.MethodHead {
		return errors.New("method 只能是 GET 或 HEAD")
	}
	for _, s := range p.ExpectStatus {
		if s < 100 || s > 599 {
			return fmt.Errorf("expect_status 不合法：%d", s)
		}
	}
	if len(p.Contains) > 256 {
		return errors.New("contains 不能超过 256 字符")
	}
	if p.TimeoutSec <= 0 {
		p.TimeoutSec = 10
	}
	if p.TimeoutSec > 30 {
		p.TimeoutSec = 30
	}
	return nil
}

func (hostHTTPCheckAction) Name() string { return "host.http_check" }
func (hostHTTPCheckAction) Spec() ActionSpec {
	return ActionSpec{
		Name: "host.http_check", Title: "HTTP 健康检查", Channel: "platform", Risk: RiskRead, ReadOnly: true, Permission: PermRead,
		Description: "从平台侧发起 HTTP(S) 请求（GET/HEAD），校验状态码（默认 200-399）与可选的响应内容关键字，常用作验证步骤。",
		Params:      hostSchema(`{"type":"object","required":["url"],"properties":{"url":{"type":"string"},"method":{"type":"string","enum":["GET","HEAD"]},"expect_status":{"type":"array","items":{"type":"integer"}},"contains":{"type":"string"},"timeout_sec":{"type":"integer","maximum":30},"insecure":{"type":"boolean"}},"additionalProperties":false}`),
	}
}
func (a hostHTTPCheckAction) Validate(ctx context.Context, ec *ExecContext, params json.RawMessage) (json.RawMessage, string, error) {
	var p hostHTTPCheckParams
	if err := hostDecode(params, &p); err != nil {
		return nil, "", err
	}
	if err := p.normalise(); err != nil {
		return nil, "", err
	}
	return hostEncode(p), RiskRead, nil
}
func (a hostHTTPCheckAction) Describe(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, string, string, error) {
	var p hostHTTPCheckParams
	if err := hostDecode(params, &p); err != nil {
		return "", "", "", err
	}
	if err := p.normalise(); err != nil {
		return "", "", "", err
	}
	flag := "-sS -o /dev/null -w '%{http_code}\\n'"
	if p.Method == http.MethodHead {
		flag = "-sS -I"
	}
	if p.Insecure {
		flag += " -k"
	}
	cmd := fmt.Sprintf("curl %s --max-time %d %s", flag, p.TimeoutSec, hostShellQuote(p.URL))
	return "HTTP 检查 " + p.URL, fmt.Sprintf("%s %s（平台侧发起，期望状态 %s）", p.Method, p.URL, hostStatusText(p.ExpectStatus)), "在可访问该地址的机器上执行：\n" + cmd, nil
}

func hostStatusText(s []int) string {
	if len(s) == 0 {
		return "200-399"
	}
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, "/")
}

func (a hostHTTPCheckAction) Execute(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, error) {
	var p hostHTTPCheckParams
	if err := hostDecode(params, &p); err != nil {
		return "", err
	}
	if err := p.normalise(); err != nil {
		return "", err
	}
	timeout := time.Duration(p.TimeoutSec) * time.Second
	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "xingshu-aiops-check/1.0")
	start := time.Now()
	resp, err := hostHTTPDo(req, p.Insecure, timeout)
	if err != nil {
		return "", fmt.Errorf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, hostMaxOutput))
	elapsed := time.Since(start).Round(time.Millisecond)
	snippet := string(body)
	if len(snippet) > 2048 {
		snippet = snippet[:2048] + "..."
	}
	out := hostBound(fmt.Sprintf("%s %s -> %d（耗时 %s）\n%s", p.Method, p.URL, resp.StatusCode, elapsed, snippet), false)
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	if len(p.ExpectStatus) > 0 {
		ok = false
		for _, s := range p.ExpectStatus {
			if s == resp.StatusCode {
				ok = true
			}
		}
	}
	if !ok {
		return out, fmt.Errorf("状态码 %d 不符合期望 %s", resp.StatusCode, hostStatusText(p.ExpectStatus))
	}
	if p.Contains != "" && p.Method == http.MethodGet && !strings.Contains(string(body), p.Contains) {
		return out, fmt.Errorf("响应中未包含 %q", p.Contains)
	}
	return out, nil
}
func (hostHTTPCheckAction) Rollback(context.Context, *ExecContext, json.RawMessage, string) (string, error) {
	return "", ErrNoRollback
}

// ---------------------------------------------------------------------------
// host.service

type hostServiceAction struct{}

type hostServiceParams struct {
	hostTarget
	Name string `json:"name"`
	Op   string `json:"op"` // start | stop | restart | status
}

func (p *hostServiceParams) normalise() error {
	if err := p.check(); err != nil {
		return err
	}
	p.Name = strings.TrimSpace(p.Name)
	if !reHostService.MatchString(p.Name) {
		return fmt.Errorf("服务名不合法：%q（仅允许字母、数字和 _ . @ : -）", p.Name)
	}
	p.Op = strings.ToLower(strings.TrimSpace(p.Op))
	switch p.Op {
	case "start", "stop", "restart", "status":
	default:
		return errors.New("op 只能是 start、stop、restart、status")
	}
	return nil
}

func (p hostServiceParams) command() string {
	n := hostShellQuote(p.Name)
	if p.Op == "status" {
		return "systemctl status --no-pager -l -- " + n + " | head -n 40; echo \"active=$(systemctl is-active -- " + n + ")\"; true"
	}
	return "prev=$(systemctl is-active -- " + n + " 2>/dev/null); echo \"" + hostMarkPrevState + "${prev:-unknown}\"; " +
		"systemctl " + p.Op + " -- " + n + " || exit $?; sleep 1; " +
		"echo \"active=$(systemctl is-active -- " + n + ")\"; systemctl status --no-pager -l -- " + n + " | head -n 20; true"
}

func (hostServiceAction) Name() string { return "host.service" }
func (hostServiceAction) Spec() ActionSpec {
	return ActionSpec{
		Name: "host.service", Title: "systemd 服务操作", Channel: "bastion", Risk: RiskChange, Permission: hostPermConnect,
		Description: "systemctl start/stop/restart/status 指定服务。status 为只读；其余为变更，执行前记录原状态，回滚时恢复原状态（原来运行则启动，原来停止则停止）。",
		Params:      hostSchema(`{"type":"object","required":["asset_id","name","op"],"properties":{"asset_id":{"type":"integer","minimum":1},"account_id":{"type":"integer"},"name":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,127}$"},"op":{"type":"string","enum":["start","stop","restart","status"]}},"additionalProperties":false}`),
	}
}
func (a hostServiceAction) Validate(ctx context.Context, ec *ExecContext, params json.RawMessage) (json.RawMessage, string, error) {
	var p hostServiceParams
	if err := hostDecode(params, &p); err != nil {
		return nil, "", err
	}
	if err := p.normalise(); err != nil {
		return nil, "", err
	}
	base := RiskChange
	if p.Op == "status" {
		base = RiskRead
	}
	risk, err := hostRisk(ctx, ec, p.AssetID, base)
	return hostEncode(p), risk, err
}
func (a hostServiceAction) Describe(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, string, string, error) {
	var p hostServiceParams
	if err := hostDecode(params, &p); err != nil {
		return "", "", "", err
	}
	if err := p.normalise(); err != nil {
		return "", "", "", err
	}
	verb := map[string]string{"start": "启动", "stop": "停止", "restart": "重启", "status": "查看状态"}[p.Op]
	manual := fmt.Sprintf("通过堡垒机登录资产 #%d 后执行：\nsystemctl is-active %s   # 记录原状态\nsystemctl %s %s\nsystemctl status %s", p.AssetID, hostShellQuote(p.Name), p.Op, hostShellQuote(p.Name), hostShellQuote(p.Name))
	return fmt.Sprintf("%s服务 %s（主机 #%d）", verb, p.Name, p.AssetID), p.command(), manual, nil
}
func (a hostServiceAction) Execute(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, error) {
	var p hostServiceParams
	if err := hostDecode(params, &p); err != nil {
		return "", err
	}
	if err := p.normalise(); err != nil {
		return "", err
	}
	return hostRun(ctx, ec, p.hostTarget, a.Name(), p.command(), 2*time.Minute)
}
func (a hostServiceAction) Rollback(ctx context.Context, ec *ExecContext, params json.RawMessage, executeOutput string) (string, error) {
	var p hostServiceParams
	if err := hostDecode(params, &p); err != nil {
		return "", err
	}
	if err := p.normalise(); err != nil {
		return "", err
	}
	if p.Op == "status" {
		return "", ErrNoRollback
	}
	prev, ok := hostMarker(executeOutput, hostMarkPrevState)
	var op string
	switch {
	case !ok:
		return "", ErrNoRollback
	case prev == "active" || prev == "reloading" || prev == "activating":
		op = "start"
	case prev == "inactive" || prev == "failed" || prev == "deactivating":
		op = "stop"
	default: // unknown: the unit did not exist / state unreadable
		return "", ErrNoRollback
	}
	n := hostShellQuote(p.Name)
	cmd := "systemctl " + op + " -- " + n + " || exit $?; echo \"active=$(systemctl is-active -- " + n + ")\"; true"
	out, err := hostRun(ctx, ec, p.hostTarget, a.Name()+".rollback", cmd, 2*time.Minute)
	return fmt.Sprintf("恢复服务 %s 原状态（%s）：systemctl %s\n%s", p.Name, prev, op, out), err
}

// ---------------------------------------------------------------------------
// host.exec

type hostExecAction struct{}

type hostExecParams struct {
	hostTarget
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout_sec,omitempty"` // default 60, max 1800
}

func (p *hostExecParams) normalise() error {
	if err := p.check(); err != nil {
		return err
	}
	p.Command = strings.TrimSpace(p.Command)
	if p.Command == "" {
		return errors.New("command 不能为空")
	}
	if len(p.Command) > hostMaxCommandLen {
		return fmt.Errorf("command 不能超过 %d 字节", hostMaxCommandLen)
	}
	if strings.ContainsRune(p.Command, 0) {
		return errors.New("command 不能包含空字符")
	}
	if p.TimeoutSec <= 0 {
		p.TimeoutSec = 60
	}
	if p.TimeoutSec > 1800 {
		p.TimeoutSec = 1800
	}
	return nil
}

func (hostExecAction) Name() string { return "host.exec" }
func (hostExecAction) Spec() ActionSpec {
	return ActionSpec{
		Name: "host.exec", Title: "执行任意命令", Channel: "bastion", Risk: RiskHigh, Permission: hostPermConnect,
		Description: "经堡垒机在目标主机执行任意 shell 命令（高风险：必须审批，受堡垒机命令过滤与会话录像约束，无自动回滚）。仅当没有更具体的动作时使用。",
		Params:      hostSchema(`{"type":"object","required":["asset_id","command"],"properties":{"asset_id":{"type":"integer","minimum":1},"account_id":{"type":"integer"},"command":{"type":"string","maxLength":4096},"timeout_sec":{"type":"integer","maximum":1800}},"additionalProperties":false}`),
	}
}
func (a hostExecAction) Validate(ctx context.Context, ec *ExecContext, params json.RawMessage) (json.RawMessage, string, error) {
	var p hostExecParams
	if err := hostDecode(params, &p); err != nil {
		return nil, "", err
	}
	if err := p.normalise(); err != nil {
		return nil, "", err
	}
	if _, err := hostResolve(ctx, ec, p.AssetID); err != nil {
		return nil, "", err
	}
	return hostEncode(p), RiskHigh, nil
}
func (a hostExecAction) Describe(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, string, string, error) {
	var p hostExecParams
	if err := hostDecode(params, &p); err != nil {
		return "", "", "", err
	}
	if err := p.normalise(); err != nil {
		return "", "", "", err
	}
	first := strings.SplitN(p.Command, "\n", 2)[0]
	if len(first) > 60 {
		first = first[:60] + "..."
	}
	return fmt.Sprintf("在主机 #%d 执行命令：%s", p.AssetID, ai.RedactSecrets(first)), ai.RedactSecrets(p.Command), hostManual(p.AssetID, p.Command), nil
}
func (a hostExecAction) Execute(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, error) {
	var p hostExecParams
	if err := hostDecode(params, &p); err != nil {
		return "", err
	}
	if err := p.normalise(); err != nil {
		return "", err
	}
	return hostRun(ctx, ec, p.hostTarget, a.Name(), p.Command, time.Duration(p.TimeoutSec)*time.Second)
}
func (hostExecAction) Rollback(context.Context, *ExecContext, json.RawMessage, string) (string, error) {
	return "", ErrNoRollback
}

// ---------------------------------------------------------------------------
// host.deploy

type hostDeployAction struct{}

type hostDeployParams struct {
	hostTarget
	Mode string `json:"mode"` // run | compose

	// mode=run
	Name    string            `json:"name,omitempty"`
	Image   string            `json:"image,omitempty"`
	Ports   []string          `json:"ports,omitempty"`   // "8080:80", "127.0.0.1:8080:80/tcp"
	Env     map[string]string `json:"env,omitempty"`     // KEY -> value (quoted)
	Volumes []string          `json:"volumes,omitempty"` // "/data/nginx:/usr/share/nginx/html:ro", "vol:/data"
	Restart string            `json:"restart,omitempty"` // no | always | unless-stopped | on-failure; default unless-stopped
	Args    []string          `json:"args,omitempty"`    // container command args (quoted)

	// mode=compose
	ProjectDir     string `json:"project_dir,omitempty"`     // absolute path
	File           string `json:"file,omitempty"`            // default docker-compose.yml
	ComposeContent string `json:"compose_content,omitempty"` // optional new compose file content
	Pull           bool   `json:"pull,omitempty"`
}

func hostSafeValue(field, v string) error {
	if strings.ContainsAny(v, "\x00\n\r") {
		return fmt.Errorf("%s 不能包含换行或空字符", field)
	}
	if len(v) > 4096 {
		return fmt.Errorf("%s 过长", field)
	}
	return nil
}

func (p *hostDeployParams) normalise() error {
	if err := p.check(); err != nil {
		return err
	}
	p.Mode = strings.ToLower(strings.TrimSpace(p.Mode))
	switch p.Mode {
	case "run":
		if !reHostContainer.MatchString(p.Name) {
			return fmt.Errorf("容器名不合法：%q", p.Name)
		}
		if strings.HasSuffix(p.Name, ".aiops-prev") {
			return errors.New("容器名不能以 .aiops-prev 结尾")
		}
		if len(p.Image) > 255 || !reHostImage.MatchString(p.Image) {
			return fmt.Errorf("镜像名不合法：%q", p.Image)
		}
		for _, m := range p.Ports {
			if !reHostPortMap.MatchString(m) {
				return fmt.Errorf("端口映射不合法：%q", m)
			}
		}
		for k, v := range p.Env {
			if !reHostEnvKey.MatchString(k) {
				return fmt.Errorf("环境变量名不合法：%q", k)
			}
			if err := hostSafeValue("环境变量 "+k, v); err != nil {
				return err
			}
		}
		for _, v := range p.Volumes {
			if !reHostVolume.MatchString(v) || strings.Contains(v, "..") {
				return fmt.Errorf("挂载不合法：%q", v)
			}
		}
		if p.Restart == "" {
			p.Restart = "unless-stopped"
		}
		switch p.Restart {
		case "no", "always", "unless-stopped", "on-failure":
		default:
			return fmt.Errorf("restart 不合法：%q", p.Restart)
		}
		if len(p.Args) > 32 {
			return errors.New("args 最多 32 个")
		}
		for i, a := range p.Args {
			if err := hostSafeValue(fmt.Sprintf("args[%d]", i), a); err != nil {
				return err
			}
		}
		if p.ProjectDir != "" || p.File != "" || p.ComposeContent != "" {
			return errors.New("mode=run 不接受 compose 参数")
		}
	case "compose":
		if !reHostAbsPath.MatchString(p.ProjectDir) || strings.Contains(p.ProjectDir, "..") || p.ProjectDir == "/" {
			return fmt.Errorf("project_dir 必须是绝对路径且不含 ..：%q", p.ProjectDir)
		}
		if p.File == "" {
			p.File = "docker-compose.yml"
		}
		if !reHostFileName.MatchString(p.File) {
			return fmt.Errorf("compose 文件名不合法：%q", p.File)
		}
		if len(p.ComposeContent) > hostMaxComposeLen {
			return errors.New("compose_content 过大")
		}
		if p.Image != "" || p.Name != "" || len(p.Ports) > 0 || len(p.Env) > 0 || len(p.Volumes) > 0 || len(p.Args) > 0 {
			return errors.New("mode=compose 只接受 project_dir/file/compose_content/pull")
		}
	default:
		return errors.New("mode 只能是 run 或 compose")
	}
	return nil
}

func (p hostDeployParams) runArgs() string {
	var b strings.Builder
	b.WriteString("docker run -d --name " + hostShellQuote(p.Name) + " --restart " + hostShellQuote(p.Restart))
	for _, m := range p.Ports {
		b.WriteString(" -p " + hostShellQuote(m))
	}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(" -e " + hostShellQuote(k+"="+p.Env[k]))
	}
	for _, v := range p.Volumes {
		b.WriteString(" -v " + hostShellQuote(v))
	}
	b.WriteString(" " + hostShellQuote(p.Image))
	for _, a := range p.Args {
		b.WriteString(" " + hostShellQuote(a))
	}
	return b.String()
}

func (p hostDeployParams) command() string {
	if p.Mode == "run" {
		n := hostShellQuote(p.Name)
		prev := hostShellQuote(p.Name + ".aiops-prev")
		return "docker pull " + hostShellQuote(p.Image) + " || exit $?; " +
			"if docker inspect " + n + " >/dev/null 2>&1; then " +
			"docker rm -f " + prev + " >/dev/null 2>&1; " +
			"echo \"" + hostMarkPrevImage + "$(docker inspect -f '{{.Config.Image}}' " + n + ")\"; " +
			"docker stop " + n + " >/dev/null && docker rename " + n + " " + prev + " || exit $?; " +
			"echo \"" + hostMarkPrevContainer + p.Name + ".aiops-prev\"; fi; " +
			p.runArgs() + " || { rc=$?; docker rm -f " + n + " >/dev/null 2>&1; " +
			"if docker inspect " + prev + " >/dev/null 2>&1; then docker rename " + prev + " " + n + " && docker start " + n + " >/dev/null; echo '已自动恢复原容器'; fi; exit $rc; }; " +
			"sleep 2; docker ps -a --filter " + hostShellQuote("name=^/"+p.Name+"$") + " --format '{{.Names}} {{.Image}} {{.Status}}'"
	}
	var b strings.Builder
	b.WriteString("cd " + hostShellQuote(p.ProjectDir) + " || exit $?; F=" + hostShellQuote(p.File) + "; ")
	b.WriteString("DC='docker compose'; docker compose version >/dev/null 2>&1 || DC=docker-compose; ")
	b.WriteString("if [ -f \"$F\" ]; then cp -p \"$F\" \"$F.aiops-bak\" || exit $?; echo \"" + hostMarkComposeBackup + "$F.aiops-bak\"; else echo " + hostMarkComposeFresh + "; fi; ")
	if p.ComposeContent != "" {
		enc := base64.StdEncoding.EncodeToString([]byte(p.ComposeContent))
		b.WriteString("printf '%s' " + hostShellQuote(enc) + " | base64 -d > \"$F.aiops-new\" && mv \"$F.aiops-new\" \"$F\" || exit $?; ")
	}
	if p.Pull {
		b.WriteString("$DC -f \"$F\" pull || exit $?; ")
	}
	b.WriteString("$DC -f \"$F\" up -d || exit $?; $DC -f \"$F\" ps")
	return b.String()
}

func (hostDeployAction) Name() string { return "host.deploy" }
func (hostDeployAction) Spec() ActionSpec {
	return ActionSpec{
		Name: "host.deploy", Title: "主机容器部署", Channel: "bastion", Risk: RiskChange, Permission: hostPermConnect,
		Description: "在主机上用 docker run（mode=run：name/image/ports/env/volumes/restart/args）或 docker compose up -d（mode=compose：project_dir/file/compose_content/pull）部署。run 会先拉取镜像，再把旧容器改名为 <name>.aiops-prev 保留；compose 会先备份原文件为 <file>.aiops-bak。回滚：停止新容器并恢复旧容器/旧镜像，或恢复 compose 备份后重新 up（首次部署则 down）。",
		Params:      hostSchema(`{"type":"object","required":["asset_id","mode"],"properties":{"asset_id":{"type":"integer","minimum":1},"account_id":{"type":"integer"},"mode":{"type":"string","enum":["run","compose"]},"name":{"type":"string"},"image":{"type":"string"},"ports":{"type":"array","items":{"type":"string"}},"env":{"type":"object","additionalProperties":{"type":"string"}},"volumes":{"type":"array","items":{"type":"string"}},"restart":{"type":"string","enum":["no","always","unless-stopped","on-failure"]},"args":{"type":"array","items":{"type":"string"}},"project_dir":{"type":"string"},"file":{"type":"string"},"compose_content":{"type":"string"},"pull":{"type":"boolean"}},"additionalProperties":false}`),
	}
}
func (a hostDeployAction) Validate(ctx context.Context, ec *ExecContext, params json.RawMessage) (json.RawMessage, string, error) {
	var p hostDeployParams
	if err := hostDecode(params, &p); err != nil {
		return nil, "", err
	}
	if err := p.normalise(); err != nil {
		return nil, "", err
	}
	risk, err := hostRisk(ctx, ec, p.AssetID, RiskChange)
	return hostEncode(p), risk, err
}
func (a hostDeployAction) Describe(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, string, string, error) {
	var p hostDeployParams
	if err := hostDecode(params, &p); err != nil {
		return "", "", "", err
	}
	if err := p.normalise(); err != nil {
		return "", "", "", err
	}
	cmd := p.command()
	if p.Mode == "run" {
		manual := fmt.Sprintf("通过堡垒机登录资产 #%d 后执行：\ndocker pull %s\ndocker inspect -f '{{.Config.Image}}' %s   # 记录旧镜像（如存在）\ndocker stop %s && docker rename %s %s.aiops-prev   # 保留旧容器以便回滚\n%s",
			p.AssetID, hostShellQuote(p.Image), hostShellQuote(p.Name), hostShellQuote(p.Name), hostShellQuote(p.Name), p.Name, ai.RedactSecrets(p.runArgs()))
		return fmt.Sprintf("在主机 #%d 部署容器 %s（%s）", p.AssetID, p.Name, p.Image), ai.RedactSecrets(cmd), manual, nil
	}
	preview := ai.RedactSecrets(cmd)
	if p.ComposeContent != "" {
		preview += "\n\n# 新 " + p.File + " 内容：\n" + ai.RedactSecrets(p.ComposeContent)
	}
	manual := fmt.Sprintf("通过堡垒机登录资产 #%d 后执行：\ncd %s\ncp -p %s %s.aiops-bak   # 备份原文件（如存在）\n", p.AssetID, hostShellQuote(p.ProjectDir), hostShellQuote(p.File), p.File)
	if p.ComposeContent != "" {
		manual += "# 将预览中的新内容写入 " + p.File + "\n"
	}
	if p.Pull {
		manual += "docker compose -f " + hostShellQuote(p.File) + " pull\n"
	}
	manual += "docker compose -f " + hostShellQuote(p.File) + " up -d"
	return fmt.Sprintf("在主机 #%d 以 compose 部署 %s/%s", p.AssetID, p.ProjectDir, p.File), preview, manual, nil
}
func (a hostDeployAction) Execute(ctx context.Context, ec *ExecContext, params json.RawMessage) (string, error) {
	var p hostDeployParams
	if err := hostDecode(params, &p); err != nil {
		return "", err
	}
	if err := p.normalise(); err != nil {
		return "", err
	}
	return hostRun(ctx, ec, p.hostTarget, a.Name(), p.command(), 15*time.Minute)
}
func (a hostDeployAction) Rollback(ctx context.Context, ec *ExecContext, params json.RawMessage, executeOutput string) (string, error) {
	var p hostDeployParams
	if err := hostDecode(params, &p); err != nil {
		return "", err
	}
	if err := p.normalise(); err != nil {
		return "", err
	}
	var cmd, summary string
	if p.Mode == "run" {
		n := hostShellQuote(p.Name)
		prev := hostShellQuote(p.Name + ".aiops-prev")
		cmd = "docker rm -f " + n + " >/dev/null 2>&1; " +
			"if docker inspect " + prev + " >/dev/null 2>&1; then docker rename " + prev + " " + n + " && docker start " + n + " >/dev/null || exit $?; echo '已恢复旧容器'; else echo '无旧容器，已停止并删除新容器'; fi; " +
			"docker ps -a --filter " + hostShellQuote("name=^/"+p.Name+"$") + " --format '{{.Names}} {{.Image}} {{.Status}}'"
		if img, ok := hostMarker(executeOutput, hostMarkPrevImage); ok && img != "" {
			summary = fmt.Sprintf("停止新容器 %s，恢复旧容器（镜像 %s）", p.Name, img)
		} else {
			summary = fmt.Sprintf("停止并删除新容器 %s（部署前不存在同名容器）", p.Name)
		}
	} else {
		cmd = "cd " + hostShellQuote(p.ProjectDir) + " || exit $?; F=" + hostShellQuote(p.File) + "; " +
			"DC='docker compose'; docker compose version >/dev/null 2>&1 || DC=docker-compose; "
		if _, ok := hostMarker(executeOutput, hostMarkComposeBackup); ok {
			cmd += "[ -f \"$F.aiops-bak\" ] || { echo '备份文件不存在'; exit 1; }; mv \"$F.aiops-bak\" \"$F\" || exit $?; $DC -f \"$F\" up -d --remove-orphans || exit $?; $DC -f \"$F\" ps"
			summary = "恢复 compose 文件备份并重新 up -d"
		} else if strings.Contains(executeOutput, hostMarkComposeFresh) {
			cmd += "$DC -f \"$F\" down || exit $?; echo '首次部署，已 down'"
			summary = "首次部署，执行 compose down 停止"
		} else {
			return "", ErrNoRollback
		}
	}
	out, err := hostRun(ctx, ec, p.hostTarget, a.Name()+".rollback", cmd, 10*time.Minute)
	return summary + "\n" + out, err
}
