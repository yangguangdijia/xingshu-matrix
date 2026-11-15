package bastion

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
)

func TestResolveApprovalTimeoutPriority(t *testing.T) {
	cases := []struct {
		name         string
		rule, global int
		want         time.Duration
	}{
		{"规则优先于全局", 30, 60, 30 * time.Minute},
		{"规则为 0 用全局", 0, 60, 60 * time.Minute},
		{"都未设置用 10 分钟", 0, 0, 10 * time.Minute},
		{"规则上限 1440", 1440, 5, 1440 * time.Minute},
		{"规则下限 1", 1, 5, time.Minute},
		{"规则越界视为未设置", 1441, 20, 20 * time.Minute},
		{"规则负数视为未设置", -3, 20, 20 * time.Minute},
		{"全局越界回落 10 分钟", 0, 5000, 10 * time.Minute},
		{"全局负数回落 10 分钟", 0, -1, 10 * time.Minute},
	}
	for _, tc := range cases {
		if got := resolveApprovalTimeout(tc.rule, tc.global); got != tc.want {
			t.Errorf("%s: resolveApprovalTimeout(%d,%d)=%v want %v", tc.name, tc.rule, tc.global, got, tc.want)
		}
	}
}

func TestCommandApprovalTimeoutWithoutSettingsService(t *testing.T) {
	m := &Module{} // 无 Deps / 设置服务：规则值生效，否则回落 10 分钟
	if got := m.commandApprovalTimeout(context.Background(), &CommandEvent{ApprovalTimeoutMinutes: 45}); got != 45*time.Minute {
		t.Fatalf("rule timeout = %v", got)
	}
	if got := m.commandApprovalTimeout(context.Background(), &CommandEvent{}); got != CommandApprovalTimeout {
		t.Fatalf("default timeout = %v", got)
	}
	if got := m.commandApprovalTimeout(context.Background(), nil); got != CommandApprovalTimeout {
		t.Fatalf("nil request timeout = %v", got)
	}
}

func TestApprovalRequestCarriesRuleTimeout(t *testing.T) {
	p := NewInputProcessor([]FilterRule{{ID: 9, Name: "重启审批", Re: regexp.MustCompile(`^reboot`), Action: "approve", ApprovalTimeoutMinutes: 90}})
	p.Input([]byte("reboot\r"))
	req := p.TakeApprovalRequest()
	if req == nil || req.RuleID != 9 || req.ApprovalTimeoutMinutes != 90 {
		t.Fatalf("approval request = %+v", req)
	}
}

func TestFilterInApprovalTimeoutValidation(t *testing.T) {
	in := filterIn{Name: "x", Pattern: "^reboot", Action: "approve", ApprovalTimeoutMinutes: 1441}
	if in.check() == nil {
		t.Fatal("1441 should be rejected")
	}
	in = filterIn{Name: "x", Pattern: "^reboot", Action: "approve", ApprovalTimeoutMinutes: -1}
	if in.check() == nil {
		t.Fatal("negative should be rejected")
	}
	in = filterIn{Name: "x", Pattern: "^reboot", Action: "approve", ApprovalTimeoutMinutes: 120}
	if err := in.check(); err != nil || in.ApprovalTimeoutMinutes != 120 {
		t.Fatalf("approve 120: err=%v v=%d", err, in.ApprovalTimeoutMinutes)
	}
	in = filterIn{Name: "x", Pattern: "^reboot", Action: "deny", ApprovalTimeoutMinutes: 120}
	if err := in.check(); err != nil || in.ApprovalTimeoutMinutes != 0 {
		t.Fatalf("non-approve rule must store 0: err=%v v=%d", err, in.ApprovalTimeoutMinutes)
	}
}

func TestBastionSettingsValidator(t *testing.T) {
	v := settings.Validators[SettingsKey]
	if v == nil {
		t.Fatal("validator not registered")
	}
	for raw, okWant := range map[string]bool{
		`{"command_approval_timeout_minutes":30}`:   true,
		`{"command_approval_timeout_minutes":0}`:    true,
		`{}`:                                        true,
		`{"command_approval_timeout_minutes":1441}`: false,
		`{"command_approval_timeout_minutes":-5}`:   false,
		`"x"`: false,
	} {
		if err := v(json.RawMessage(raw)); (err == nil) != okWant {
			t.Errorf("%s: err=%v", raw, err)
		}
	}
	if got := viewOf(Settings{}).EffectiveCommandApprovalTimeoutMinutes; got != 10 {
		t.Fatalf("effective default = %d", got)
	}
	if got := viewOf(Settings{CommandApprovalTimeoutMinutes: 25}).EffectiveCommandApprovalTimeoutMinutes; got != 25 {
		t.Fatalf("effective = %d", got)
	}
}
