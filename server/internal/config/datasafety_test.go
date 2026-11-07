package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func baseEnv(extra map[string]string) lookupFunc {
	m := map[string]string{
		"MATRIX_JWT_SECRET":     strings.Repeat("j", 40),
		"MATRIX_ENCRYPTION_KEY": strings.Repeat("ab", 32),
	}
	for k, v := range extra {
		m[k] = v
	}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func loadWith(t *testing.T, doc string, env lookupFunc) (*Config, error) {
	t.Helper()
	cfg := Default()
	if doc != "" {
		if err := yaml.Unmarshal([]byte(doc), cfg); err != nil {
			return nil, err
		}
	}
	if err := applyEnv(cfg, env); err != nil {
		return nil, err
	}
	return cfg, cfg.finalize()
}

func TestDataSafetyDefaults(t *testing.T) {
	cfg, err := loadWith(t, "", baseEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Backup
	if !b.Enabled || !b.BeforeMigrate || b.Keep != 7 || b.Schedule != "0 2 * * *" || b.PgDump != "pg_dump" {
		t.Fatalf("defaults: %+v", b)
	}
	if cfg.Archive.Dir == "" || cfg.ObjectStore.Enabled() {
		t.Fatalf("archive/object store defaults: %+v %+v", cfg.Archive, cfg.ObjectStore)
	}
}

func TestDataSafetyEnvAndYAML(t *testing.T) {
	cfg, err := loadWith(t, "backup:\n  enabled: false\n  keep: 3\n", baseEnv(map[string]string{
		"MATRIX_S3_ENDPOINT":              "http://minio:9000",
		"MATRIX_S3_BUCKET":                "matrix",
		"MATRIX_S3_PATH_STYLE":            "true",
		"MATRIX_BACKUP_BEFORE_MIGRATE":    "false",
		"MATRIX_ARCHIVE_DIR":              "/data/archive",
		"MATRIX_ENCRYPTION_PREVIOUS_KEYS": strings.Repeat("cd", 32) + ", " + strings.Repeat("ef", 32),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Enabled || cfg.Backup.Keep != 3 || cfg.Backup.BeforeMigrate {
		t.Fatalf("backup: %+v", cfg.Backup)
	}
	if !cfg.ObjectStore.Enabled() || !cfg.ObjectStore.PathStyle || cfg.ObjectStore.Region != "us-east-1" {
		t.Fatalf("object store: %+v", cfg.ObjectStore)
	}
	if cfg.Archive.Dir != "/data/archive" {
		t.Fatal(cfg.Archive.Dir)
	}
	keys, err := cfg.PreviousEncryptionKeyBytes()
	if err != nil || len(keys) != 2 {
		t.Fatalf("previous keys: %v %d", err, len(keys))
	}
}

func TestDataSafetyValidation(t *testing.T) {
	if _, err := loadWith(t, "", baseEnv(map[string]string{"MATRIX_ENCRYPTION_PREVIOUS_KEYS": "short"})); err == nil {
		t.Fatal("bad previous key accepted")
	}
	if _, err := loadWith(t, "", baseEnv(map[string]string{"MATRIX_S3_ENDPOINT": "http://x"})); err == nil {
		t.Fatal("endpoint without bucket accepted")
	}
	if _, err := loadWith(t, "", baseEnv(map[string]string{"MATRIX_S3_ENDPOINT": "minio:9000", "MATRIX_S3_BUCKET": "b"})); err == nil {
		t.Fatal("endpoint without scheme accepted")
	}
	if _, err := loadWith(t, "", baseEnv(map[string]string{"MATRIX_BACKUP_KEEP": "x"})); err == nil {
		t.Fatal("bad keep accepted")
	}
}
