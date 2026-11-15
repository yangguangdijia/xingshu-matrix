package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationSource is one module's set of migrations. Files live at the root of
// FS and are named NNNN_description.up.sql (e.g. 0001_init.up.sql). Versions are
// tracked per module, so modules never collide on numbering.
type MigrationSource struct {
	Module string
	FS     fs.FS
}

// Migration is one parsed migration file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

var migrationFile = regexp.MustCompile(`^(\d+)_([A-Za-z0-9_\-]+)\.up\.sql$`)

// advisory lock key: "xingshu-matrix migrations"
const migrationLockKey int64 = 0x58534d4d4947 // "XSMMIG"

// ParseMigrations reads and orders the migrations in fsys (non-matching files are ignored).
func ParseMigrations(fsys fs.FS) ([]Migration, error) {
	if fsys == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v <= 0 {
			return nil, fmt.Errorf("migration %s: version must be > 0", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %s and %s", v, prev, e.Name())
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: m[2], SQL: string(b), Checksum: migrationChecksum(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func normalizedMigrationSQL(sql []byte) []byte {
	normalized := bytes.ReplaceAll(sql, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
}

func checksumBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func migrationChecksum(sql []byte) string {
	return checksumBytes(normalizedMigrationSQL(sql))
}

func migrationChecksumMatches(stored string, sql []byte) bool {
	normalized := normalizedMigrationSQL(sql)
	crlf := bytes.ReplaceAll(normalized, []byte("\n"), []byte("\r\n"))
	candidates := [][]byte{normalized, crlf}
	// A few Windows-built migrations were recorded with one extra trailing
	// newline. Accept that line-ending variation, but not arbitrary whitespace
	// changes in the SQL body.
	candidates = append(candidates,
		append(append([]byte(nil), normalized...), '\n'),
		append(append([]byte(nil), crlf...), '\r', '\n'),
	)
	for _, candidate := range candidates {
		if stored == checksumBytes(candidate) {
			return true
		}
	}
	return false
}

// MigrateOptions are optional hooks for MigrateWith.
type MigrateOptions struct {
	// BeforeApply, when set, is called once with every pending migration
	// ("module/0007_name") after all sources were checked and before the first
	// one is applied, while the migration lock is held. It is not called when
	// nothing is pending. A non-nil error aborts the run without applying anything.
	BeforeApply func(ctx context.Context, pending []string) error
}

// Migrate applies pending migrations of all sources in order, each migration in
// its own transaction, under a PostgreSQL advisory lock so several replicas can
// start concurrently. Already-applied migrations whose content changed are an error.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, sources ...MigrationSource) error {
	return MigrateWith(ctx, pool, log, MigrateOptions{}, sources...)
}

// pendingSource is the not-yet-applied part of one source.
type pendingSource struct {
	module string
	migs   []Migration
}

// planPending returns the migrations of one module that are not in applied
// (version -> stored checksum), failing when an applied one was modified.
func planPending(module string, migs []Migration, applied map[int]string) ([]Migration, error) {
	var out []Migration
	for _, m := range migs {
		if sum, ok := applied[m.Version]; ok {
			if !migrationChecksumMatches(sum, []byte(m.SQL)) {
				return nil, fmt.Errorf("module %s migration %04d_%s was modified after being applied (checksum mismatch); add a new migration instead", module, m.Version, m.Name)
			}
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// pendingNames renders the plan as "module/0007_name" in apply order.
func pendingNames(plan []pendingSource) []string {
	var names []string
	for _, p := range plan {
		for _, m := range p.migs {
			names = append(names, fmt.Sprintf("%s/%04d_%s", p.module, m.Version, m.Name))
		}
	}
	return names
}

// MigrateWith is Migrate with optional hooks (see MigrateOptions). All
// sources are checked first, so a checksum mismatch aborts before BeforeApply
// runs and before anything is applied.
func MigrateWith(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, opts MigrateOptions, sources ...MigrationSource) error {
	if log == nil {
		log = slog.Default()
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		module     TEXT        NOT NULL,
		version    INTEGER     NOT NULL,
		name       TEXT        NOT NULL,
		checksum   TEXT        NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (module, version))`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var plan []pendingSource
	for _, src := range sources {
		migs, err := ParseMigrations(src.FS)
		if err != nil {
			return fmt.Errorf("module %s: %w", src.Module, err)
		}
		if len(migs) == 0 {
			continue
		}
		applied := map[int]string{}
		rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations WHERE module=$1`, src.Module)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v int
			var sum string
			if err := rows.Scan(&v, &sum); err != nil {
				rows.Close()
				return err
			}
			applied[v] = sum
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		pending, err := planPending(src.Module, migs, applied)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			plan = append(plan, pendingSource{module: src.Module, migs: pending})
		}
	}
	if len(plan) == 0 {
		return nil
	}
	if opts.BeforeApply != nil {
		if err := opts.BeforeApply(ctx, pendingNames(plan)); err != nil {
			return err
		}
	}

	for _, p := range plan {
		for _, m := range p.migs {
			err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, m.SQL); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(module, version, name, checksum) VALUES ($1,$2,$3,$4)`,
					p.module, m.Version, m.Name, m.Checksum)
				return err
			})
			if err != nil {
				return fmt.Errorf("module %s migration %04d_%s: %w", p.module, m.Version, m.Name, err)
			}
			log.Info("migration applied", "module", p.module, "version", m.Version, "name", m.Name)
		}
	}
	return nil
}
