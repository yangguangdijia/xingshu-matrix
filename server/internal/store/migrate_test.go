package store

import (
	"bytes"
	"testing"
	"testing/fstest"
)

func TestParseMigrationsChecksumIsLineEndingStable(t *testing.T) {
	lfSQL := []byte("CREATE TABLE example (id BIGINT PRIMARY KEY);\n")
	crlfSQL := bytes.ReplaceAll(lfSQL, []byte("\n"), []byte("\r\n"))

	lf, err := ParseMigrations(fstest.MapFS{
		"0001_example.up.sql": &fstest.MapFile{Data: lfSQL},
	})
	if err != nil {
		t.Fatal(err)
	}
	crlf, err := ParseMigrations(fstest.MapFS{
		"0001_example.up.sql": &fstest.MapFile{Data: crlfSQL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lf[0].Checksum != crlf[0].Checksum {
		t.Fatalf("line-ending variants have different checksums: LF %s, CRLF %s", lf[0].Checksum, crlf[0].Checksum)
	}
	if !migrationChecksumMatches(checksumBytes(crlfSQL), lfSQL) {
		t.Fatal("does not recognize an already-applied CRLF checksum")
	}
}

func TestMigrationChecksumRejectsSQLChanges(t *testing.T) {
	applied := []byte("CREATE TABLE example (id BIGINT PRIMARY KEY);\n")
	changed := []byte("CREATE TABLE example (id BIGINT NOT NULL PRIMARY KEY);\n")
	stored := checksumBytes(bytes.ReplaceAll(applied, []byte("\n"), []byte("\r\n")))

	if migrationChecksumMatches(stored, changed) {
		t.Fatal("accepted a migration whose SQL content changed")
	}
}

func TestMigrationChecksumAcceptsOneTrailingBlankLine(t *testing.T) {
	sql := []byte("CREATE TABLE example (id BIGINT PRIMARY KEY);\n")
	withBlankLine := append(append([]byte(nil), sql...), '\n')
	if !migrationChecksumMatches(checksumBytes(withBlankLine), sql) {
		t.Fatal("did not recognize a historical trailing blank line")
	}

	withTwoBlankLines := append(append([]byte(nil), sql...), '\n', '\n')
	if migrationChecksumMatches(checksumBytes(withTwoBlankLines), sql) {
		t.Fatal("accepted multiple trailing blank lines")
	}
}

func TestCoreMigrationMatchesAppliedWindowsChecksum(t *testing.T) {
	migrations, err := ParseMigrations(CoreMigrations())
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 || migrations[0].Version != 1 || migrations[0].Name != "core" {
		t.Fatal("core migration 0001_core is missing")
	}

	const appliedChecksum = "e447a8a5c836fa4a9445d6e274a7a9899edb95cbf02f22824546c298c9eaf61e"
	if !migrationChecksumMatches(appliedChecksum, []byte(migrations[0].SQL)) {
		t.Fatal("core migration 0001_core no longer matches the applied SQL")
	}
}

func TestPlanPendingSkipsAppliedAndRejectsModified(t *testing.T) {
	migs := []Migration{
		{Version: 1, Name: "a", SQL: "SELECT 1;\n", Checksum: migrationChecksum([]byte("SELECT 1;\n"))},
		{Version: 2, Name: "b", SQL: "SELECT 2;\n", Checksum: migrationChecksum([]byte("SELECT 2;\n"))},
		{Version: 3, Name: "c", SQL: "SELECT 3;\n", Checksum: migrationChecksum([]byte("SELECT 3;\n"))},
	}
	got, err := planPending("core", migs, map[int]string{1: migs[0].Checksum})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Version != 2 || got[1].Version != 3 {
		t.Fatalf("pending = %+v", got)
	}
	if _, err := planPending("core", migs, map[int]string{2: migs[0].Checksum}); err == nil {
		t.Fatal("modified applied migration not rejected")
	}
	none, err := planPending("core", migs, map[int]string{1: migs[0].Checksum, 2: migs[1].Checksum, 3: migs[2].Checksum})
	if err != nil || len(none) != 0 {
		t.Fatalf("expected nothing pending, got %v %v", none, err)
	}
}

func TestPendingNamesOrder(t *testing.T) {
	plan := []pendingSource{
		{module: "core", migs: []Migration{{Version: 12, Name: "jobs"}}},
		{module: "cicd", migs: []Migration{{Version: 3, Name: "targets"}, {Version: 4, Name: "x"}}},
	}
	got := pendingNames(plan)
	want := []string{"core/0012_jobs", "cicd/0003_targets", "cicd/0004_x"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
