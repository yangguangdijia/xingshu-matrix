package bastion

// 堡垒机设置：目前只有命令审批的全局默认超时。
// 存放在平台设置服务（settings 表）的键 bastion.settings 中，通过 GET/PUT /bastion/settings 读写；
// 平台通用设置接口写该键时同样经过 validateBastionSettings 校验。
//
// 命令审批超时取值优先级（resolveApprovalTimeout）：
//   1. 命中的命令过滤规则（action=approve）上的 approval_timeout_minutes（1–1440）；
//   2. 规则为 0 时取堡垒机全局默认 command_approval_timeout_minutes（1–1440）；
//   3. 都未配置（或设置读取失败 / 值非法）时用 CommandApprovalTimeout（10 分钟）。

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
)

// SettingsKey 是堡垒机设置在平台设置服务中的键。
const SettingsKey = "bastion.settings"

// MaxApprovalTimeoutMinutes 是审批超时的上限（24 小时）。
const MaxApprovalTimeoutMinutes = 1440

// Settings 是堡垒机模块设置。
type Settings struct {
	// CommandApprovalTimeoutMinutes 命令审批全局默认超时（分钟），1–1440；0 表示未设置（使用 10 分钟）。
	CommandApprovalTimeoutMinutes int `json:"command_approval_timeout_minutes"`
}

func (s Settings) validate() error {
	if s.CommandApprovalTimeoutMinutes < 0 || s.CommandApprovalTimeoutMinutes > MaxApprovalTimeoutMinutes {
		return httpx.Validation("命令审批默认超时需为 1–1440 分钟（0 表示使用系统默认 10 分钟）")
	}
	return nil
}

func validateBastionSettings(raw json.RawMessage) error {
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return httpx.Validation("堡垒机设置格式不正确")
	}
	return s.validate()
}

func init() {
	settings.Validators[SettingsKey] = validateBastionSettings
}

// resolveApprovalTimeout 按「规则 > 全局默认 > 10 分钟」返回实际超时；越界值视为未设置。
func resolveApprovalTimeout(ruleMinutes, globalMinutes int) time.Duration {
	if ruleMinutes >= 1 && ruleMinutes <= MaxApprovalTimeoutMinutes {
		return time.Duration(ruleMinutes) * time.Minute
	}
	if globalMinutes >= 1 && globalMinutes <= MaxApprovalTimeoutMinutes {
		return time.Duration(globalMinutes) * time.Minute
	}
	return CommandApprovalTimeout
}

// loadSettings 读取堡垒机设置；未设置或读取失败时返回零值（调用方回落到默认）。
func (m *Module) loadSettings(ctx context.Context) (Settings, error) {
	var s Settings
	if m.d == nil || m.d.Settings == nil {
		return s, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := m.d.Settings.Get(cctx, SettingsKey, &s)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return Settings{}, nil
		}
		return Settings{}, err
	}
	if s.validate() != nil {
		return Settings{}, nil
	}
	return s, nil
}

// commandApprovalTimeout 返回本次审批请求的实际等待时间。
func (m *Module) commandApprovalTimeout(ctx context.Context, req *CommandEvent) time.Duration {
	rule := 0
	if req != nil {
		rule = req.ApprovalTimeoutMinutes
	}
	if rule >= 1 && rule <= MaxApprovalTimeoutMinutes {
		return resolveApprovalTimeout(rule, 0)
	}
	s, _ := m.loadSettings(ctx)
	return resolveApprovalTimeout(rule, s.CommandApprovalTimeoutMinutes)
}

type settingsView struct {
	Settings
	// EffectiveCommandApprovalTimeoutMinutes 规则未单独设置时实际使用的分钟数。
	EffectiveCommandApprovalTimeoutMinutes int `json:"effective_command_approval_timeout_minutes"`
}

func viewOf(s Settings) settingsView {
	return settingsView{Settings: s, EffectiveCommandApprovalTimeoutMinutes: int(resolveApprovalTimeout(0, s.CommandApprovalTimeoutMinutes) / time.Minute)}
}

func (m *Module) getSettings(c *gin.Context) {
	s, err := m.loadSettings(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, viewOf(s))
}

func (m *Module) putSettings(c *gin.Context) {
	var in Settings
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.validate(); err != nil {
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
	audit.SetResource(c, "bastion_settings", SettingsKey)
	audit.SetDetail(c, gin.H{"command_approval_timeout_minutes": in.CommandApprovalTimeoutMinutes})
	if _, err := m.d.Settings.Put(c.Request.Context(), SettingsKey, raw, false, "堡垒机设置（命令审批默认超时）", by); err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, viewOf(in))
}
