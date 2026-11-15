package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Failure modes of the pre-migration snapshot
// (MATRIX_BACKUP_BEFORE_MIGRATE_ON_FAILURE).
const (
	FailureAbort = "abort"
	FailureWarn  = "warn"
)

// ParseFailureMode parses the pre-migration failure mode. "" means abort.
func ParseFailureMode(v string) (warnOnly bool, err error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", FailureAbort:
		return false, nil
	case FailureWarn:
		return true, nil
	}
	return false, fmt.Errorf("MATRIX_BACKUP_BEFORE_MIGRATE_ON_FAILURE 只能为 abort 或 warn，当前为 %q", v)
}

// preMigrate is the testable core of PreMigrateHook.
type preMigrate struct {
	enabled  bool
	warnOnly bool
	log      *slog.Logger
	// state reports whether the database already holds platform data
	// (schema_migrations has rows) and whether platform_backups exists.
	state func(ctx context.Context) (existing, recorded bool, err error)
	// snapshot takes the dump; recorded=false means platform_backups does not
	// exist yet (upgrade from before 0011) and the dump is kept without a row.
	snapshot func(ctx context.Context, recorded bool) (file string, err error)
}

// PreMigrateHook returns a store.MigrateOptions.BeforeApply callback. When the
// migrator finds pending migrations on a database that already has data, it
// takes a pre_migrate backup through r before anything is applied:
//
//   - r.BeforeMigrate=false (MATRIX_BACKUP_BEFORE_MIGRATE=false): skipped.
//   - fresh database (schema_migrations empty): skipped, nothing to protect.
//   - platform_backups missing (upgrading from a version before it existed):
//     the dump is still taken and kept in Dir / the object store, only not listed.
//   - failure: the migration is aborted with a Chinese error, unless
//     r.PreMigrateWarnOnly (MATRIX_BACKUP_BEFORE_MIGRATE_ON_FAILURE=warn), in
//     which case a warning is logged and the migration continues.
//
// A nil r yields a hook that only logs that the snapshot is unavailable.
func PreMigrateHook(r *Runner) func(ctx context.Context, pending []string) error {
	if r == nil {
		return func(context.Context, []string) error {
			slog.Default().Warn("pre-migrate backup unavailable: backup runner not configured")
			return nil
		}
	}
	h := &preMigrate{
		enabled:  r.BeforeMigrate,
		warnOnly: r.PreMigrateWarnOnly,
		log:      r.log(),
		state:    r.dbState,
		snapshot: r.preMigrateSnapshot,
	}
	return h.run
}

func (h *preMigrate) run(ctx context.Context, pending []string) error {
	if len(pending) == 0 {
		return nil
	}
	if !h.enabled {
		h.log.Info("pre-migrate backup disabled", "pending", len(pending))
		return nil
	}
	existing, recorded, err := h.state(ctx)
	if err == nil && !existing {
		h.log.Info("pre-migrate backup skipped: new database", "pending", len(pending))
		return nil
	}
	file := ""
	if err == nil {
		h.log.Info("taking pre-migrate backup", "pending", pending, "recorded", recorded)
		file, err = h.snapshot(ctx, recorded)
	}
	if err != nil {
		if h.warnOnly {
			h.log.Warn("pre-migrate backup failed, continuing migration (on_failure=warn)", "pending", len(pending), "err", err)
			return nil
		}
		return fmt.Errorf("迁移前自动备份失败，已中止数据库迁移（待执行 %d 项迁移：%s）：%w。请排查后重试；确认无需备份时可设置 MATRIX_BACKUP_BEFORE_MIGRATE=false 跳过，或设置 MATRIX_BACKUP_BEFORE_MIGRATE_ON_FAILURE=warn 改为仅告警",
			len(pending), summarize(pending, 5), err)
	}
	h.log.Info("pre-migrate backup succeeded", "file", file, "recorded", recorded)
	return nil
}

func summarize(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, "、")
	}
	return strings.Join(names[:max], "、") + fmt.Sprintf(" 等 %d 项", len(names))
}

func (r *Runner) dbState(ctx context.Context) (existing, recorded bool, err error) {
	if r.DB == nil {
		return false, false, errors.New("备份未连接数据库")
	}
	err = r.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations), to_regclass('platform_backups') IS NOT NULL`).Scan(&existing, &recorded)
	if err != nil {
		return false, false, fmt.Errorf("检查数据库状态失败: %w", err)
	}
	return existing, recorded, nil
}

func (r *Runner) preMigrateSnapshot(ctx context.Context, recorded bool) (string, error) {
	if recorded {
		rec, err := r.Run(ctx, KindPreMigrate, 0)
		if err != nil {
			return "", err
		}
		return rec.File, nil
	}
	if !r.mu.TryLock() {
		return "", ErrBusy
	}
	defer r.mu.Unlock()
	res, err := r.dumpToFile(ctx)
	if err != nil {
		return "", err
	}
	if r.remote() {
		if err := r.upload(ctx, res); err != nil {
			return "", err
		}
	}
	return res.File, nil
}
