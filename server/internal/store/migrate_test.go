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
