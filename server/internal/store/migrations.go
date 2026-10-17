package store

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var coreMigrations embed.FS

// CoreMigrations returns the core schema (users, rbac, sessions, audit,
// approvals, settings, ai). It is always applied before module migrations.
func CoreMigrations() fs.FS {
	sub, err := fs.Sub(coreMigrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}
