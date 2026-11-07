package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ObjectStoreConfig points at an S3-compatible bucket (AWS S3, MinIO, Ceph RGW,
// OSS/COS in S3 mode). Empty Endpoint or Bucket disables object storage and the
// local directories below are used instead.
type ObjectStoreConfig struct {
	// Endpoint is the base URL, e.g. "https://s3.cn-north-1.amazonaws.com.cn" or "http://minio:9000".
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// Prefix is prepended to every object key (e.g. "matrix/").
	Prefix string `yaml:"prefix"`
	// PathStyle uses http://endpoint/bucket/key instead of virtual-hosted style (MinIO needs true).
	PathStyle bool `yaml:"path_style"`
}

// Enabled reports whether an S3-compatible store is configured.
func (o ObjectStoreConfig) Enabled() bool { return o.Endpoint != "" && o.Bucket != "" }

// ArchiveConfig controls where expired audit / command records are archived
// before deletion when no object store is configured.
type ArchiveConfig struct {
	Dir string `yaml:"dir"`
}

// BackupConfig controls platform (PostgreSQL) backups.
type BackupConfig struct {
	// Enabled turns the scheduled backup on (manual and pre-upgrade backups work regardless).
	Enabled bool `yaml:"enabled"`
	// Schedule is a 5-field cron expression in the server time zone (TZ). Default "0 2 * * *".
	Schedule string `yaml:"schedule"`
	// Keep is how many automatic (scheduled / pre-upgrade) backups to retain. Default 7.
	Keep int `yaml:"keep"`
	// Dir is the local backup directory used when no object store is configured.
	Dir string `yaml:"dir"`
	// BeforeMigrate takes a snapshot when the migrator finds pending migrations on an existing database.
	BeforeMigrate bool `yaml:"before_migrate"`
	// PgDump is the pg_dump binary; when it is missing or incompatible a COPY-based logical export is used.
	PgDump string `yaml:"pg_dump"`
	// PgRestore is the pg_restore binary used by `matrix-server restore` for pg_dump archives.
	PgRestore string `yaml:"pg_restore"`

	// set is true once Enabled/BeforeMigrate were given explicitly (yaml or env).
	enabledSet, beforeSet bool
}

// UnmarshalYAML records whether the booleans were set so defaults can apply.
func (b *BackupConfig) UnmarshalYAML(unmarshal func(any) error) error {
	type plain BackupConfig
	var raw map[string]any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	var p plain
	if err := unmarshal(&p); err != nil {
		return err
	}
	*b = BackupConfig(p)
	_, b.enabledSet = raw["enabled"]
	_, b.beforeSet = raw["before_migrate"]
	return nil
}

func applyDataSafetyEnv(c *Config, env lookupFunc) error {
	str := func(key string, dst *string) {
		if v, ok := env(key); ok {
			*dst = v
		}
	}
	boolean := func(key string, dst *bool, set *bool) error {
		v, ok := env(key)
		if !ok {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*dst = b
		if set != nil {
			*set = true
		}
		return nil
	}
	if v, ok := env("MATRIX_ENCRYPTION_PREVIOUS_KEYS"); ok {
		c.Security.EncryptionPreviousKeys = splitList(v)
	}
	str("MATRIX_S3_ENDPOINT", &c.ObjectStore.Endpoint)
	str("MATRIX_S3_REGION", &c.ObjectStore.Region)
	str("MATRIX_S3_BUCKET", &c.ObjectStore.Bucket)
	str("MATRIX_S3_ACCESS_KEY", &c.ObjectStore.AccessKey)
	str("MATRIX_S3_SECRET_KEY", &c.ObjectStore.SecretKey)
	str("MATRIX_S3_PREFIX", &c.ObjectStore.Prefix)
	if err := boolean("MATRIX_S3_PATH_STYLE", &c.ObjectStore.PathStyle, nil); err != nil {
		return err
	}
	str("MATRIX_ARCHIVE_DIR", &c.Archive.Dir)
	str("MATRIX_BACKUP_DIR", &c.Backup.Dir)
	str("MATRIX_BACKUP_SCHEDULE", &c.Backup.Schedule)
	str("MATRIX_PG_DUMP", &c.Backup.PgDump)
	str("MATRIX_PG_RESTORE", &c.Backup.PgRestore)
	if v, ok := env("MATRIX_BACKUP_KEEP"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("MATRIX_BACKUP_KEEP: %w", err)
		}
		c.Backup.Keep = n
	}
	if err := boolean("MATRIX_BACKUP_ENABLED", &c.Backup.Enabled, &c.Backup.enabledSet); err != nil {
		return err
	}
	return boolean("MATRIX_BACKUP_BEFORE_MIGRATE", &c.Backup.BeforeMigrate, &c.Backup.beforeSet)
}

func (c *Config) finalizeDataSafety() []error {
	var errs []error
	if c.Archive.Dir == "" {
		c.Archive.Dir = "/var/lib/matrix/archive"
	}
	b := &c.Backup
	if b.Dir == "" {
		b.Dir = "/var/lib/matrix/backups"
	}
	if strings.TrimSpace(b.Schedule) == "" {
		b.Schedule = "0 2 * * *"
	}
	if b.Keep <= 0 {
		b.Keep = 7
	}
	if b.Keep > 365 {
		errs = append(errs, errors.New("backup.keep must be <= 365"))
	}
	if !b.enabledSet {
		b.Enabled = true
	}
	if !b.beforeSet {
		b.BeforeMigrate = true
	}
	if b.PgDump == "" {
		b.PgDump = "pg_dump"
	}
	if b.PgRestore == "" {
		b.PgRestore = "pg_restore"
	}
	if c.ObjectStore.Endpoint != "" || c.ObjectStore.Bucket != "" {
		if !c.ObjectStore.Enabled() {
			errs = append(errs, errors.New("object_store.endpoint and object_store.bucket must be set together (MATRIX_S3_ENDPOINT / MATRIX_S3_BUCKET)"))
		} else if !strings.HasPrefix(c.ObjectStore.Endpoint, "http://") && !strings.HasPrefix(c.ObjectStore.Endpoint, "https://") {
			errs = append(errs, errors.New("object_store.endpoint must start with http:// or https://"))
		}
		if c.ObjectStore.Region == "" {
			c.ObjectStore.Region = "us-east-1"
		}
	}
	if _, err := c.PreviousEncryptionKeyBytes(); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// PreviousEncryptionKeyBytes decodes security.encryption_previous_keys.
func (c *Config) PreviousEncryptionKeyBytes() ([][]byte, error) {
	var out [][]byte
	for i, k := range c.Security.EncryptionPreviousKeys {
		b, ok := decodeKey32(k)
		if !ok {
			return nil, fmt.Errorf("security.encryption_previous_keys[%d] must decode (base64 or hex) to exactly 32 bytes", i)
		}
		out = append(out, b)
	}
	return out, nil
}

func decodeKey32(k string) ([]byte, bool) {
	k = strings.TrimSpace(k)
	if b, err := hex.DecodeString(k); err == nil && len(b) == 32 {
		return b, true
	}
	if b, err := base64.StdEncoding.DecodeString(k); err == nil && len(b) == 32 {
		return b, true
	}
	return nil, false
}
