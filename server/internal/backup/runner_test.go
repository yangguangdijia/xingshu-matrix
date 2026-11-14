package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fakePgDump installs a pg_dump shell script first on PATH. It records its
// argv and PGPASSWORD into dir/args and dir/pw.
func fakePgDump(t *testing.T, body string) string {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + bin + "/args\"\nprintf '%s' \"$PGPASSWORD\" > \"" + bin + "/pw\"\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "pg_dump"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

func fixedNow() time.Time { return time.Date(2026, 11, 14, 2, 0, 5, 0, time.UTC) }

func TestDumpSuccess(t *testing.T) {
	bin := fakePgDump(t, `printf 'PGDMP-fake-archive'`)
	dir := t.TempDir()
	r := &Runner{DSN: "postgres://matrix:s3cr%40t@db:5432/matrix?sslmode=disable", Dir: dir, Now: fixedNow}
	res, err := r.dumpToFile(context.Background())
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if res.File != "xs-20261114T020005Z.dump" {
		t.Errorf("file = %q", res.File)
	}
	want := sha256.Sum256([]byte("PGDMP-fake-archive"))
	if res.SHA256 != hex.EncodeToString(want[:]) || res.Size != int64(len("PGDMP-fake-archive")) {
		t.Errorf("sha/size = %s/%d", res.SHA256, res.Size)
	}
	b, err := os.ReadFile(filepath.Join(dir, res.File))
	if err != nil || string(b) != "PGDMP-fake-archive" {
		t.Fatalf("file content %q err %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, res.File)); st.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v", st.Mode().Perm())
	}
	args, _ := os.ReadFile(filepath.Join(bin, "args"))
	if strings.Contains(string(args), "s3cr") {
		t.Errorf("password leaked into argv: %s", args)
	}
	if !strings.Contains(string(args), "--format=custom") || !strings.Contains(string(args), "--dbname=postgres://matrix@db:5432/matrix?sslmode=disable") {
		t.Errorf("args = %s", args)
	}
	if pw, _ := os.ReadFile(filepath.Join(bin, "pw")); string(pw) != "s3cr@t" {
		t.Errorf("PGPASSWORD = %q", pw)
	}

	// Second dump in the same second gets a distinct name.
	res2, err := r.dumpToFile(context.Background())
	if err != nil || res2.File == res.File {
		t.Fatalf("second dump: %v %q", err, res2)
	}
}

func TestDumpFailure(t *testing.T) {
	fakePgDump(t, `echo 'pg_dump: error: connection failed for password s3cret' >&2; printf partial; exit 1`)
	dir := t.TempDir()
	r := &Runner{DSN: "host=db user=matrix password='s3cret' dbname=matrix", Dir: dir, Now: fixedNow}
	_, err := r.dumpToFile(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "pg_dump 执行失败") || !strings.Contains(err.Error(), "connection failed") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("password leaked into error: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Errorf("partial file left behind: %v", ents)
	}
}

func TestDumpMissingBinary(t *testing.T) {
	r := &Runner{DSN: "postgres://u@h/d", Dir: t.TempDir(), PgDump: "/nonexistent/pg_dump"}
	_, err := r.dumpToFile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "未找到 pg_dump") {
		t.Fatalf("err = %v", err)
	}
	if err := r.checkTool(); err == nil {
		t.Fatal("checkTool should fail")
	}
}

func TestConnArgs(t *testing.T) {
	cases := []struct{ in, conn, pw string }{
		{"postgres://a:b@h:5432/d?sslmode=disable", "postgres://a@h:5432/d?sslmode=disable", "b"},
		{"postgresql://a@h/d?password=x&sslmode=require", "postgresql://a@h/d?sslmode=require", "x"},
		{"host=h user=a password=pw dbname=d", "host=h user=a dbname=d", "pw"},
		{`host=h password='p w\'q' dbname='my db'`, `host=h dbname='my db'`, "p w'q"},
		{"host=h dbname=d", "host=h dbname=d", ""},
	}
	for _, c := range cases {
		conn, pw, err := ConnArgs(c.in)
		if err != nil || conn != c.conn || pw != c.pw {
			t.Errorf("ConnArgs(%q) = %q, %q, %v; want %q, %q", c.in, conn, pw, err, c.conn, c.pw)
		}
	}
	if _, _, err := ConnArgs(""); err == nil {
		t.Error("empty DSN should fail")
	}
}

func TestPGEnvReplacesPassword(t *testing.T) {
	t.Setenv("PGPASSWORD", "old")
	env := PGEnv("new")
	n := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PGPASSWORD=") {
			n++
			if e != "PGPASSWORD=new" {
				t.Errorf("got %s", e)
			}
		}
	}
	if n != 1 {
		t.Errorf("PGPASSWORD entries = %d", n)
	}
}

// TestSlotClaimAndRun needs a migrated PostgreSQL (MATRIX_TEST_DSN).
func TestSlotClaimAndRun(t *testing.T) {
	dsn := os.Getenv("MATRIX_TEST_DSN")
	if dsn == "" {
		t.Skip("MATRIX_TEST_DSN not set; skipping slot claim test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `SELECT 1 FROM platform_backups LIMIT 1`); err != nil {
		t.Skipf("platform_backups missing: %v", err)
	}
	fakePgDump(t, `printf 'PGDMP'`)
	slot := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	key := SlotKey(slot)
	defer pool.Exec(ctx, `DELETE FROM platform_backups WHERE schedule_slot=$1`, key)

	a := &Runner{DB: pool, DSN: dsn, Dir: t.TempDir()}
	b := &Runner{DB: pool, DSN: dsn, Dir: t.TempDir()}
	rec, ran, err := a.RunSlot(ctx, slot)
	if err != nil || !ran || rec.Status != StatusSucceeded {
		t.Fatalf("first replica: ran=%v rec=%+v err=%v", ran, rec, err)
	}
	if _, ran, err := b.RunSlot(ctx, slot); err != nil || ran {
		t.Fatalf("second replica must not run the same slot: ran=%v err=%v", ran, err)
	}
	_ = os.Remove(filepath.Join(a.Dir, rec.File))
}
