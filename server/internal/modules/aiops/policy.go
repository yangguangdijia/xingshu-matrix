package aiops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// PolicySettingsKey is the settings key holding the auto-remediation policy.
const PolicySettingsKey = "aiops.policy"

// Policy is the AI operations automation policy (GET/PUT /aiops/policy).
// The zero value (everything off) is the default.
type Policy struct {
	AutoInvestigate PolicyAutoInvestigate `json:"auto_investigate"`
	AutoExecute     PolicyAutoExecute     `json:"auto_execute"`
	ChangeWindow    PolicyChangeWindow    `json:"change_window"`
	ModelID         string                `json:"model_id"`
}

// PolicyAutoInvestigate: start an investigation automatically for matching alerts.
type PolicyAutoInvestigate struct {
	Enabled    bool     `json:"enabled"`
	Severities []string `json:"severities"`
	Envs       []string `json:"envs"`
}

// PolicyAutoExecute: allow whitelisted (auto_execute) runbooks to run unattended.
type PolicyAutoExecute struct {
	Enabled    bool `json:"enabled"`
	MaxPerHour int  `json:"max_per_hour"`
}

// PolicyChangeWindow restricts high-risk execution to a weekly window.
type PolicyChangeWindow struct {
	Enabled  bool   `json:"enabled"`
	CronLike string `json:"cron_like"` // "Mon-Fri 09:00-21:00"
}

func policyLoc() *time.Location {
	if l, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return l
	}
	return time.FixedZone("CST", 8*3600)
}

// policyNormalize fills nil slices so the JSON is stable.
func policyNormalize(p *Policy) {
	if p.AutoInvestigate.Severities == nil {
		p.AutoInvestigate.Severities = []string{}
	}
	if p.AutoInvestigate.Envs == nil {
		p.AutoInvestigate.Envs = []string{}
	}
	p.ModelID = strings.TrimSpace(p.ModelID)
	p.ChangeWindow.CronLike = strings.TrimSpace(p.ChangeWindow.CronLike)
}

// loadPolicy reads the policy; missing settings (or no settings service) means all off.
func (m *Module) loadPolicy(ctx context.Context) (Policy, error) {
	var p Policy
	if m.d == nil || m.d.Settings == nil {
		policyNormalize(&p)
		return p, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := m.d.Settings.Get(cctx, PolicySettingsKey, &p); err != nil {
		p = Policy{}
		policyNormalize(&p)
		if errors.Is(err, httpx.ErrNotFound) {
			return p, nil
		}
		return p, err
	}
	policyNormalize(&p)
	return p, nil
}

var policySeverities = map[string]bool{"critical": true, "warning": true, "info": true}

// policyValidate returns a Chinese validation error.
func policyValidate(p *Policy) error {
	policyNormalize(p)
	for i, s := range p.AutoInvestigate.Severities {
		s = strings.ToLower(strings.TrimSpace(s))
		if !policySeverities[s] {
			return httpx.Validation("不支持的告警级别：" + s + "（可选 critical/warning/info）")
		}
		p.AutoInvestigate.Severities[i] = s
	}
	for i, e := range p.AutoInvestigate.Envs {
		e = strings.TrimSpace(e)
		if e == "" || len(e) > 64 {
			return httpx.Validation("自动排查环境名称无效")
		}
		p.AutoInvestigate.Envs[i] = e
	}
	if p.AutoInvestigate.Enabled && len(p.AutoInvestigate.Severities) == 0 {
		return httpx.Validation("开启自动排查时至少选择一个告警级别")
	}
	if p.AutoExecute.MaxPerHour < 0 || p.AutoExecute.MaxPerHour > 100 {
		return httpx.Validation("每小时自动处置次数上限须在 0–100 之间")
	}
	if p.AutoExecute.Enabled && p.AutoExecute.MaxPerHour < 1 {
		return httpx.Validation("开启自动处置时每小时次数上限至少为 1")
	}
	if p.ChangeWindow.Enabled || p.ChangeWindow.CronLike != "" {
		if _, err := policyParseWindow(p.ChangeWindow.CronLike); err != nil {
			return httpx.Validation("变更窗口格式错误，应为 \"Mon-Fri 09:00-21:00\"：" + err.Error())
		}
	}
	if len(p.ModelID) > 128 {
		return httpx.Validation("模型 ID 过长")
	}
	return nil
}

// ---------- change window ----------

var policyDays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

type policyWindow struct {
	days       [7]bool
	start, end int // minutes of day; end exclusive; start > end = overnight
}

// policyParseWindow parses "Mon-Fri 09:00-21:00", "Sat,Sun 00:00-24:00",
// "* 22:00-06:00" (overnight). Several windows may be joined with ";".
func policyParseWindows(s string) ([]policyWindow, error) {
	var out []policyWindow
	for _, part := range strings.Split(s, ";") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		w, err := policyParseWindow(part)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil, errors.New("为空")
	}
	return out, nil
}

func policyParseWindow(s string) (policyWindow, error) {
	var w policyWindow
	if strings.Contains(s, ";") {
		ws, err := policyParseWindows(s)
		if err != nil {
			return w, err
		}
		return ws[0], nil
	}
	f := strings.Fields(s)
	if len(f) != 2 {
		return w, errors.New("需要“星期 时间段”两部分")
	}
	if err := policyParseDays(f[0], &w.days); err != nil {
		return w, err
	}
	tr := strings.SplitN(f[1], "-", 2)
	if len(tr) != 2 {
		return w, errors.New("时间段应为 HH:MM-HH:MM")
	}
	var err error
	if w.start, err = policyParseClock(tr[0]); err != nil {
		return w, err
	}
	if w.end, err = policyParseClock(tr[1]); err != nil {
		return w, err
	}
	if w.start == w.end || w.start == 24*60 {
		return w, errors.New("开始与结束时间无效")
	}
	return w, nil
}

func policyParseDays(s string, days *[7]bool) error {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "*" || s == "daily" || s == "everyday" {
		for i := range days {
			days[i] = true
		}
		return nil
	}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if a, b, ok := strings.Cut(tok, "-"); ok {
			da, ok1 := policyDays[policyDayKey(a)]
			db, ok2 := policyDays[policyDayKey(b)]
			if !ok1 || !ok2 {
				return fmt.Errorf("无法识别的星期：%s", tok)
			}
			for d := da; ; d = (d + 1) % 7 {
				days[d] = true
				if d == db {
					break
				}
			}
			continue
		}
		d, ok := policyDays[policyDayKey(tok)]
		if !ok {
			return fmt.Errorf("无法识别的星期：%s", tok)
		}
		days[d] = true
	}
	return nil
}

func policyDayKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 3 {
		s = s[:3]
	}
	return s
}

func policyParseClock(s string) (int, error) {
	var h, mi int
	s = strings.TrimSpace(s)
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &mi); err != nil || n != 2 || len(s) < 4 || len(s) > 5 {
		return 0, fmt.Errorf("无法识别的时间：%s", s)
	}
	if h < 0 || h > 24 || mi < 0 || mi > 59 || (h == 24 && mi != 0) {
		return 0, fmt.Errorf("无法识别的时间：%s", s)
	}
	return h*60 + mi, nil
}

func (w policyWindow) contains(t time.Time) bool {
	min := t.Hour()*60 + t.Minute()
	wd := t.Weekday()
	if w.start < w.end {
		return w.days[wd] && min >= w.start && min < w.end
	}
	// overnight: the day names the window's start day
	if min >= w.start {
		return w.days[wd]
	}
	if min < w.end {
		return w.days[(wd+6)%7]
	}
	return false
}

// inChangeWindow reports whether t (evaluated in Asia/Shanghai) is inside the
// change window. A disabled window always allows; an unparsable one never does.
func inChangeWindow(p Policy, t time.Time) bool {
	if !p.ChangeWindow.Enabled {
		return true
	}
	ws, err := policyParseWindows(p.ChangeWindow.CronLike)
	if err != nil {
		return false
	}
	lt := t.In(policyLoc())
	for _, w := range ws {
		if w.contains(lt) {
			return true
		}
	}
	return false
}

// ---------- HTTP ----------

func (m *Module) handleGetPolicy(c *gin.Context) {
	p, err := m.loadPolicy(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"policy": p, "in_change_window": inChangeWindow(p, time.Now())})
}

func (m *Module) handlePutPolicy(c *gin.Context) {
	var in Policy
	if !httpx.Bind(c, &in) {
		return
	}
	if err := policyValidate(&in); err != nil {
		httpx.Fail(c, err)
		return
	}
	if m.d == nil || m.d.Settings == nil {
		httpx.Fail(c, httpx.NewError(503, httpx.CodeUnavailable, "设置服务不可用"))
		return
	}
	raw, _ := json.Marshal(in)
	var by int64
	if p := rbac.PrincipalFrom(c); p != nil {
		by = p.UserID
	}
	audit.SetResource(c, "aiops_policy", PolicySettingsKey)
	audit.SetDetail(c, in)
	if _, err := m.d.Settings.Put(c.Request.Context(), PolicySettingsKey, raw, false, "AI 运维自动化策略（自动排查/自动处置/变更窗口）", by); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"policy": in, "in_change_window": inChangeWindow(in, time.Now())})
}

// policyOnboardInput is POST /hosts/onboard. Credentials go only to the
// bastion (encrypted custody); they are never echoed, logged or audited.
type policyOnboardInput struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	Port       int    `json:"port"`
	Env        string `json:"env"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	Node       string `json:"node"`
}

func policyValidateOnboard(in *policyOnboardInput) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Address = strings.TrimSpace(in.Address)
	in.Env = strings.TrimSpace(in.Env)
	in.Username = strings.TrimSpace(in.Username)
	if in.Address == "" {
		return httpx.Validation("主机地址不能为空")
	}
	if net.ParseIP(in.Address) == nil && !policyValidHostname(in.Address) {
		return httpx.Validation("主机地址格式错误")
	}
	if in.Name == "" {
		in.Name = in.Address
	}
	if len([]rune(in.Name)) > 128 {
		return httpx.Validation("主机名称过长")
	}
	if in.Port == 0 {
		in.Port = 22
	}
	if in.Port < 1 || in.Port > 65535 {
		return httpx.Validation("端口须在 1–65535 之间")
	}
	if in.Env == "" {
		return httpx.Validation("请选择环境")
	}
	if in.Username == "" {
		return httpx.Validation("登录账号不能为空")
	}
	hasPw, hasKey := in.Password != "", strings.TrimSpace(in.PrivateKey) != ""
	if hasPw == hasKey {
		return httpx.Validation("请提供密码或私钥其中之一")
	}
	return nil
}

func policyValidHostname(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// policyRedact strips secrets from a message (defensive; providers should not echo them).
func policyRedact(msg string, secrets ...string) string {
	for _, s := range secrets {
		if strings.TrimSpace(s) != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

func (m *Module) handleOnboardHost(c *gin.Context) {
	var in policyOnboardInput
	if !httpx.Bind(c, &in) {
		return
	}
	// The secrets live only in `in`; clear them whatever happens.
	defer func() { in.Password, in.PrivateKey = "", "" }()
	if err := policyValidateOnboard(&in); err != nil {
		httpx.Fail(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	if !p.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("无权操作该环境："+in.Env))
		return
	}
	authType := "password"
	if in.PrivateKey != "" {
		authType = "private_key"
	}
	audit.SetResource(c, "aiops_host", in.Address)
	audit.SetDetail(c, gin.H{"name": in.Name, "address": in.Address, "port": in.Port, "env": in.Env,
		"username": in.Username, "auth_type": authType})
	if m.d == nil || m.d.RemoteExec == nil {
		httpx.Fail(c, httpx.NewError(503, httpx.CodeUnavailable, "堡垒机远程执行服务未启用，无法纳管主机"))
		return
	}
	ref, err := m.d.RemoteExec.OnboardAsset(c.Request.Context(), p, app.AssetOnboardRequest{
		Name: in.Name, Address: in.Address, Port: in.Port, Env: in.Env,
		Username: in.Username, Password: in.Password, PrivateKey: in.PrivateKey, Node: in.Node,
	})
	if err != nil {
		var he *httpx.Error
		if errors.As(err, &he) {
			httpx.Fail(c, httpx.NewError(he.Status, he.Code, policyRedact(he.Message, in.Password, in.PrivateKey)))
			return
		}
		if m.d.Logger != nil {
			m.d.Logger.Warn("aiops: onboard host failed", "address", in.Address, "env", in.Env,
				"err", policyRedact(err.Error(), in.Password, in.PrivateKey))
		}
		httpx.Fail(c, httpx.NewError(502, httpx.CodeUpstream, "主机纳管失败："+policyRedact(err.Error(), in.Password, in.PrivateKey)))
		return
	}
	httpx.Created(c, policyAssetView(ref))
}

// policyAssetView is the credential-free response of /hosts/onboard.
func policyAssetView(r app.AssetRef) gin.H {
	return gin.H{"asset_id": r.AssetID, "account_id": r.AccountID, "name": r.Name, "address": r.Address,
		"env": r.Env, "reachable": r.Reachable, "os": r.OS}
}
