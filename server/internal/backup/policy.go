package backup

// Policy is the effective backup configuration as shown in the console. It
// comes from the deployment config (backup.* / MATRIX_BACKUP_*) and is
// read-only at runtime.
type Policy struct {
	// ScheduleEnabled is true when scheduled backups run (backup.enabled).
	ScheduleEnabled bool `json:"schedule_enabled"`
	// Schedule is the 5-field cron expression in the server time zone.
	Schedule string `json:"schedule"`
	// Keep is how many succeeded scheduled backups are retained; manual and
	// pre_migrate backups are never pruned automatically.
	Keep int `json:"keep"`
	// BeforeMigrate: a pre_migrate backup is taken before pending migrations.
	BeforeMigrate bool `json:"before_migrate"`
	// BeforeMigrateOnFailure is "abort" or "warn".
	BeforeMigrateOnFailure string `json:"before_migrate_on_failure"`
	// Storage is "local" (backup directory on the server) or "s3".
	Storage string `json:"storage"`
	// OnlineRestore is false: restoring is an offline operation
	// (`matrix-server restore`), not exposed over HTTP.
	OnlineRestore bool `json:"online_restore"`
}

// Policy reports the runner's effective configuration.
func (r *Runner) Policy() Policy {
	p := Policy{
		ScheduleEnabled:        r.Schedule != nil,
		Keep:                   r.Keep,
		BeforeMigrate:          r.BeforeMigrate,
		BeforeMigrateOnFailure: FailureAbort,
		Storage:                "local",
	}
	if p.ScheduleEnabled {
		p.Schedule = r.ScheduleSpec
	}
	if r.PreMigrateWarnOnly {
		p.BeforeMigrateOnFailure = FailureWarn
	}
	if r.remote() {
		p.Storage = r.Store.Kind()
	}
	return p
}
