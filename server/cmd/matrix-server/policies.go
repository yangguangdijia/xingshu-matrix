package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

const (
	securityPolicyKey  = "security.policy"
	retentionPolicyKey = "audit.retention"
)

// RetentionPolicy is stored under "audit.retention". 0 days = keep forever.
type RetentionPolicy struct {
	AuditDays   int `json:"audit_days"`   // 0 or 30-3650
	CommandDays int `json:"command_days"` // bastion command records, 0 or 30-3650
}

func (p RetentionPolicy) validate() error {
	for _, v := range []struct {
		name string
		n    int
	}{{"审计日志保留天数", p.AuditDays}, {"堡垒机命令记录保留天数", p.CommandDays}} {
		if v.n != 0 && (v.n < 30 || v.n > 3650) {
			return httpx.Validation(v.name + "需为 0（永久）或 30-3650")
		}
	}
	return nil
}

// registerPolicies wires settings-backed runtime policies.
func registerPolicies(st *settings.Service) {
	auth.SetPolicySource(func(ctx context.Context) (auth.Policy, error) {
		var p auth.Policy
		err := st.Get(ctx, securityPolicyKey, &p)
		return p, err
	})
	settings.Validators[securityPolicyKey] = func(raw json.RawMessage) error {
		var p auth.Policy
		if err := json.Unmarshal(raw, &p); err != nil {
			return httpx.Validation("安全策略格式错误")
		}
		return p.Validate()
	}
	settings.OnChange[securityPolicyKey] = auth.InvalidatePolicy
	settings.Validators[retentionPolicyKey] = func(raw json.RawMessage) error {
		var p RetentionPolicy
		if err := json.Unmarshal(raw, &p); err != nil {
			return httpx.Validation("保留策略格式错误")
		}
		return p.validate()
	}
}

// purgeByRetention deletes audit and bastion command rows older than the
// configured retention, every 6 hours (and once shortly after start).
func purgeByRetention(ctx context.Context, db store.DB, st *settings.Service, log *slog.Logger) {
	run := func() {
		var p RetentionPolicy
		if err := st.Get(ctx, retentionPolicyKey, &p); err != nil || p.validate() != nil {
			return
		}
		for _, j := range []struct {
			table string
			days  int
		}{{"audit_logs", p.AuditDays}, {"bastion_commands", p.CommandDays}} {
			if j.days <= 0 {
				continue
			}
			tag, err := db.Exec(ctx, `DELETE FROM `+j.table+` WHERE created_at < now() - make_interval(days => $1)`, j.days)
			if err != nil {
				log.Warn("retention purge failed", "table", j.table, "err", err)
				continue
			}
			if n := tag.RowsAffected(); n > 0 {
				log.Info("retention purge", "table", j.table, "deleted", n, "days", j.days)
			}
		}
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			run()
			timer.Reset(6 * time.Hour)
		}
	}
}
