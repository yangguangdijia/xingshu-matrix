package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/backup/objstore"
)

// Backup kinds (platform_backups.kind).
const (
	KindManual     = "manual"
	KindScheduled  = "scheduled"
	KindPreMigrate = "pre_migrate"
)

// Backup statuses (platform_backups.status).
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// ObjectPrefix is the object-store key prefix for backups (key = ObjectPrefix + file).
const ObjectPrefix = "backups/"

// ErrBusy is returned when this process is already running a backup.
var ErrBusy = errors.New("已有平台备份正在进行，请稍后再试")

// DB is the subset of *pgxpool.Pool the runner needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Record is one platform_backups row.
type Record struct {
	ID           int64      `json:"id"`
	Kind         string     `json:"kind"`
	Status       string     `json:"status"`
	File         string     `json:"file"`
	Size         int64      `json:"size"`
	SHA256       string     `json:"sha256"`
	ScheduleSlot *string    `json:"schedule_slot"`
	Error        string     `json:"error"`
	CreatedBy    *int64     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

// Runner takes pg_dump backups, records them in platform_backups, runs the
// cron schedule and prunes old scheduled backups.
type Runner struct {
	DB DB
	// DSN is config.Database.DSN. Its password is passed to pg_dump only via PGPASSWORD.
	DSN string
	// Dir is where dumps are written (config.Backup.Dir). With a remote Store the
	// dump is staged here, uploaded, then removed.
	Dir string
	// Store is an optional object store (S3). Nil or Kind()=="local" keeps files in Dir.
	Store objstore.Store
	// Keep is how many scheduled backups to retain (PruneCandidates); <=0 disables pruning.
	Keep int
	// Schedule drives Start; nil disables scheduled backups.
	Schedule *Schedule
	// PgDump is the pg_dump binary (config.Backup.PgDump); default "pg_dump".
	PgDump string
	Log    *slog.Logger

	// Timeout bounds one pg_dump run; default 2h.
	Timeout time.Duration
	// Tick is the scheduler poll interval; default 1 minute.
	Tick time.Duration
	// Now is the clock (tests); default time.Now.
	Now func() time.Time

	// ScheduleSpec is the cron text behind Schedule (shown by the policy API).
	ScheduleSpec string
	// BeforeMigrate enables the pre_migrate snapshot (config backup.before_migrate,
	// MATRIX_BACKUP_BEFORE_MIGRATE); see PreMigrateHook.
	BeforeMigrate bool
	// PreMigrateWarnOnly makes a failed pre_migrate snapshot a warning instead
	// of aborting the migration (MATRIX_BACKUP_BEFORE_MIGRATE_ON_FAILURE=warn).
	PreMigrateWarnOnly bool

	mu sync.Mutex
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) remote() bool { return r.Store != nil && r.Store.Kind() != "local" }

// Run takes one backup synchronously: it inserts a running row, dumps, and
// marks the row succeeded or failed. userID <= 0 records no creator.
func (r *Runner) Run(ctx context.Context, kind string, userID int64) (*Record, error) {
	if !r.mu.TryLock() {
		return nil, ErrBusy
	}
	defer r.mu.Unlock()
	id, _, err := r.claim(ctx, kind, userID, "")
	if err != nil {
		return nil, err
	}
	return r.execute(ctx, id, kind)
}

// Trigger inserts a running row and runs the backup in the background,
// returning the row id immediately (used by the HTTP API).
func (r *Runner) Trigger(ctx context.Context, kind string, userID int64) (int64, error) {
	if err := r.checkTool(); err != nil {
		return 0, err
	}
	if !r.mu.TryLock() {
		return 0, ErrBusy
	}
	id, _, err := r.claim(ctx, kind, userID, "")
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	go func() {
		defer r.mu.Unlock()
		bg := context.WithoutCancel(ctx)
		if _, err := r.execute(bg, id, kind); err != nil {
			r.log().Warn("platform backup failed", "id", id, "kind", kind, "err", err)
		}
	}()
	return id, nil
}

// Start runs the scheduler until ctx is done. Every Tick it checks whether
// the next cron time has passed; the slot is claimed with
// INSERT ... ON CONFLICT (schedule_slot) DO NOTHING so that across replicas
// exactly one instance runs it.
func (r *Runner) Start(ctx context.Context) {
	if n, err := r.ReapStale(ctx); err != nil {
		r.log().Warn("reap stale platform backups", "err", err)
	} else if n > 0 {
		r.log().Warn("marked interrupted platform backups as failed", "count", n)
	}
	if r.Schedule == nil {
		return
	}
	tick := r.Tick
	if tick <= 0 {
		tick = time.Minute
	}
	next := r.Schedule.Next(r.now())
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if next.IsZero() {
			return
		}
		now := r.now()
		if now.Before(next) {
			continue
		}
		slot := next
		next = r.Schedule.Next(now)
		if _, _, err := r.RunSlot(ctx, slot); err != nil {
			r.log().Warn("scheduled platform backup failed", "slot", SlotKey(slot), "err", err)
		}
	}
}

// RunSlot claims the schedule slot and, if this instance won it, runs the
// backup and prunes. ran is false when another replica already owns the slot.
func (r *Runner) RunSlot(ctx context.Context, slot time.Time) (rec *Record, ran bool, err error) {
	r.mu.Lock() // a manual backup in progress only delays the scheduled one
	defer r.mu.Unlock()
	id, ok, err := r.claim(ctx, KindScheduled, 0, SlotKey(slot))
	if err != nil || !ok {
		return nil, false, err
	}
	rec, err = r.execute(ctx, id, KindScheduled)
	return rec, true, err
}

// ReapStale marks running rows older than the dump timeout (+10 min upload
// grace) as failed: they were left behind by a process that crashed or was
// killed mid-backup. Rows younger than that may belong to another replica
// still working and are left alone.
func (r *Runner) ReapStale(ctx context.Context) (int64, error) {
	if r.DB == nil {
		return 0, nil
	}
	age := r.Timeout
	if age <= 0 {
		age = 2 * time.Hour
	}
	age += 10 * time.Minute
	tag, err := r.DB.Exec(ctx, `UPDATE platform_backups SET status='failed', error='备份进程中断（实例重启或崩溃），本次备份未完成', finished_at=now()
		WHERE status='running' AND created_at < now() - make_interval(secs => $1)`, age.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// claim inserts the running row. With a slot it returns ok=false when the
// slot is already taken.
func (r *Runner) claim(ctx context.Context, kind string, userID int64, slot string) (int64, bool, error) {
	var by *int64
	if userID > 0 {
		by = &userID
	}
	var sl *string
	if slot != "" {
		sl = &slot
	}
	var id int64
	err := r.DB.QueryRow(ctx, `INSERT INTO platform_backups (kind, status, schedule_slot, created_by)
		VALUES ($1, 'running', $2, $3)
		ON CONFLICT (schedule_slot) DO NOTHING
		RETURNING id`, kind, sl, by).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("登记平台备份失败: %w", err)
	}
	return id, true, nil
}

func (r *Runner) execute(ctx context.Context, id int64, kind string) (*Record, error) {
	res, derr := r.dumpToFile(ctx)
	if derr == nil && r.remote() {
		derr = r.upload(ctx, res)
	}
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if derr != nil {
		msg := derr.Error()
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		if _, err := r.DB.Exec(uctx, `UPDATE platform_backups SET status='failed', error=$2, finished_at=now() WHERE id=$1`, id, msg); err != nil {
			r.log().Error("mark platform backup failed", "id", id, "err", err)
		}
		return nil, derr
	}
	if _, err := r.DB.Exec(uctx, `UPDATE platform_backups SET status='succeeded', file=$2, size=$3, sha256=$4, error='', finished_at=now() WHERE id=$1`,
		id, res.File, res.Size, res.SHA256); err != nil {
		return nil, fmt.Errorf("更新平台备份记录失败: %w", err)
	}
	r.log().Info("platform backup succeeded", "id", id, "kind", kind, "file", res.File, "size", res.Size)
	if kind == KindScheduled {
		if err := r.Prune(uctx); err != nil {
			r.log().Warn("prune platform backups", "err", err)
		}
	}
	return r.Get(uctx, id)
}

// DumpResult describes a finished dump file.
type DumpResult struct {
	File   string // base name, e.g. xs-20261114T020000Z.dump
	Path   string // full local path
	Size   int64
	SHA256 string
}

// checkTool verifies the pg_dump binary exists.
func (r *Runner) checkTool() error {
	_, err := LookTool(r.pgDump(), "pg_dump", "backup.pg_dump / MATRIX_PG_DUMP")
	return err
}

func (r *Runner) pgDump() string {
	if r.PgDump != "" {
		return r.PgDump
	}
	return "pg_dump"
}

// LookTool resolves a PostgreSQL client binary, returning a Chinese error when missing.
func LookTool(bin, name, cfgHint string) (string, error) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("未找到 %s 可执行文件（%s）：请在服务器上安装与数据库主版本一致的 PostgreSQL 客户端，或通过 %s 指定路径", name, bin, cfgHint)
	}
	return p, nil
}

// dumpToFile runs pg_dump --format=custom into Dir/xs-<UTC>.dump, hashing as it writes.
func (r *Runner) dumpToFile(ctx context.Context) (*DumpResult, error) {
	bin, err := LookTool(r.pgDump(), "pg_dump", "backup.pg_dump / MATRIX_PG_DUMP")
	if err != nil {
		return nil, err
	}
	conn, password, err := ConnArgs(r.DSN)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("创建备份目录失败: %w", err)
	}
	stamp := r.now().UTC().Format("20060102T150405Z")
	name := "xs-" + stamp + ".dump"
	path := filepath.Join(r.Dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	for i := 2; errors.Is(err, os.ErrExist) && i < 100; i++ {
		name = fmt.Sprintf("xs-%s-%d.dump", stamp, i)
		path = filepath.Join(r.Dir, name)
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("创建备份文件失败: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(path)
		}
	}()

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "--format=custom", "--no-password", "--dbname="+conn)
	cmd.Env = PGEnv(password)
	h := sha256.New()
	cw := &countWriter{}
	cmd.Stdout = io.MultiWriter(f, h, cw)
	stderr := &limitBuffer{max: 4096}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if password != "" {
			msg = strings.ReplaceAll(msg, password, "***")
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("pg_dump 超时（%s）", timeout)
		}
		if msg == "" {
			return nil, fmt.Errorf("pg_dump 执行失败: %v", err)
		}
		return nil, fmt.Errorf("pg_dump 执行失败: %v: %s", err, msg)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("写入备份文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("写入备份文件失败: %w", err)
	}
	if cw.n == 0 {
		os.Remove(path)
		ok = true
		return nil, errors.New("pg_dump 未输出任何数据")
	}
	ok = true
	return &DumpResult{File: name, Path: path, Size: cw.n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func (r *Runner) upload(ctx context.Context, res *DumpResult) error {
	f, err := os.Open(res.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := r.Store.Put(ctx, ObjectPrefix+res.File, f, res.Size); err != nil {
		os.Remove(res.Path)
		return fmt.Errorf("上传备份到对象存储失败: %w", err)
	}
	f.Close()
	os.Remove(res.Path)
	return nil
}

// Open returns a reader for a succeeded backup file.
func (r *Runner) Open(ctx context.Context, file string) (io.ReadCloser, int64, error) {
	if !validFile(file) {
		return nil, 0, fmt.Errorf("非法的备份文件名 %q", file)
	}
	if r.remote() {
		return r.Store.Get(ctx, ObjectPrefix+file)
	}
	f, err := os.Open(filepath.Join(r.Dir, file))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, objstore.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (r *Runner) remove(ctx context.Context, file string) error {
	if file == "" {
		return nil
	}
	if !validFile(file) {
		return fmt.Errorf("非法的备份文件名 %q", file)
	}
	if r.remote() {
		return r.Store.Delete(ctx, ObjectPrefix+file)
	}
	if err := os.Remove(filepath.Join(r.Dir, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func validFile(f string) bool {
	return f != "" && !strings.ContainsAny(f, `/\`) && objstore.ValidKey(f)
}

// Prune deletes succeeded scheduled backups beyond Keep (files first, then rows).
// Failed rows never count toward Keep, so a run of failures cannot evict good backups.
func (r *Runner) Prune(ctx context.Context) error {
	if r.Keep <= 0 {
		return nil
	}
	rows, err := r.DB.Query(ctx, `SELECT id, kind, created_at, file FROM platform_backups WHERE status='succeeded' AND kind=$1`, KindScheduled)
	if err != nil {
		return err
	}
	var list []BackupInfo
	files := map[int64]string{}
	for rows.Next() {
		var b BackupInfo
		var file string
		if err := rows.Scan(&b.ID, &b.Kind, &b.CreatedAt, &file); err != nil {
			rows.Close()
			return err
		}
		list = append(list, b)
		files[b.ID] = file
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var firstErr error
	for _, id := range PruneCandidates(list, r.Keep) {
		if err := r.remove(ctx, files[id]); err != nil {
			r.log().Warn("delete old platform backup file", "id", id, "file", files[id], "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := r.DB.Exec(ctx, `DELETE FROM platform_backups WHERE id=$1`, id); err != nil && firstErr == nil {
			firstErr = err
		}
		r.log().Info("pruned platform backup", "id", id, "file", files[id])
	}
	return firstErr
}

const recordCols = `id, kind, status, file, size, sha256, schedule_slot, error, created_by, created_at, finished_at`

func scanRecord(row pgx.Row) (*Record, error) {
	var b Record
	err := row.Scan(&b.ID, &b.Kind, &b.Status, &b.File, &b.Size, &b.SHA256, &b.ScheduleSlot, &b.Error, &b.CreatedBy, &b.CreatedAt, &b.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// Get loads one row; pgx.ErrNoRows when missing.
func (r *Runner) Get(ctx context.Context, id int64) (*Record, error) {
	return scanRecord(r.DB.QueryRow(ctx, `SELECT `+recordCols+` FROM platform_backups WHERE id=$1`, id))
}

// List returns rows newest first.
func (r *Runner) List(ctx context.Context, limit, offset int) ([]Record, int64, error) {
	var total int64
	if err := r.DB.QueryRow(ctx, `SELECT count(*) FROM platform_backups`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.DB.Query(ctx, `SELECT `+recordCols+` FROM platform_backups ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		b, err := scanRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *b)
	}
	return out, total, rows.Err()
}

// ConnArgs turns a DSN (URL or key=value) into a libpq connection string
// without the password, plus the password to pass via PGPASSWORD.
func ConnArgs(dsn string) (conn, password string, err error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", "", errors.New("未配置数据库连接（database.dsn / MATRIX_DB_DSN）")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", "", errors.New("数据库连接串（database.dsn）格式无效")
		}
		if u.User != nil {
			if pw, ok := u.User.Password(); ok {
				password = pw
				u.User = url.User(u.User.Username())
			}
		}
		q := u.Query()
		if pw := q.Get("password"); pw != "" {
			if password == "" {
				password = pw
			}
			q.Del("password")
			u.RawQuery = q.Encode()
		}
		return u.String(), password, nil
	}
	// keyword=value form; values may be single-quoted with \' and \\ escapes.
	var kept []string
	s := dsn
	for {
		s = strings.TrimLeft(s, " \t\n")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return "", "", errors.New("数据库连接串（database.dsn）格式无效")
		}
		key := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " \t")
		var raw, val string
		if strings.HasPrefix(s, "'") {
			var b strings.Builder
			i := 1
			closed := false
			for ; i < len(s); i++ {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					i++
					b.WriteByte(s[i])
					continue
				}
				if c == '\'' {
					closed = true
					i++
					break
				}
				b.WriteByte(c)
			}
			if !closed {
				return "", "", errors.New("数据库连接串（database.dsn）引号不匹配")
			}
			raw, val, s = s[:i], b.String(), s[i:]
		} else {
			end := strings.IndexAny(s, " \t\n")
			if end < 0 {
				end = len(s)
			}
			raw, val, s = s[:end], s[:end], s[end:]
		}
		if strings.EqualFold(key, "password") {
			password = val
			continue
		}
		kept = append(kept, key+"="+raw)
	}
	return strings.Join(kept, " "), password, nil
}

// PGEnv returns the process environment with PGPASSWORD replaced by password
// (or left as the operator set it when password is empty).
func PGEnv(password string) []string {
	env := os.Environ()
	if password == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, "PGPASSWORD=") {
			out = append(out, e)
		}
	}
	return append(out, "PGPASSWORD="+password)
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

type limitBuffer struct {
	b   []byte
	max int
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if room := l.max - len(l.b); room > 0 {
		if len(p) > room {
			l.b = append(l.b, p[:room]...)
		} else {
			l.b = append(l.b, p...)
		}
	}
	return len(p), nil
}

func (l *limitBuffer) String() string { return string(l.b) }
