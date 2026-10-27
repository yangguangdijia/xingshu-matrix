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
	if stored == checksumBytes(normalized) {
		return true
	}
	return stored == checksumBytes(bytes.ReplaceAll(normalized, []byte("\n"), []byte("\r\n")))
}

// Migrate applies pending migrations of all sources in order, each migration in
// its own transaction, under a PostgreSQL advisory lock so several replicas can
// start concurrently. Already-applied migrations whose content changed are an error.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, sources ...MigrationSource) error {
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
		for _, m := range migs {
			if sum, ok := applied[m.Version]; ok {
				if !migrationChecksumMatches(sum, []byte(m.SQL)) {
					return fmt.Errorf("module %s migration %04d_%s was modified after being applied (checksum mismatch); add a new migration instead", src.Module, m.Version, m.Name)
				}
				continue
			}
			err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, m.SQL); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(module, version, name, checksum) VALUES ($1,$2,$3,$4)`,
					src.Module, m.Version, m.Name, m.Checksum)
				return err
			})
			if err != nil {
				return fmt.Errorf("module %s migration %04d_%s: %w", src.Module, m.Version, m.Name, err)
			}
			log.Info("migration applied", "module", src.Module, "version", m.Version, "name", m.Name)
		}
	}
	return nil
}
