// Package config loads matrix-server configuration from a YAML file and
// environment variables (env wins over file, file wins over defaults).
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full server configuration.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Redis    RedisConfig    `yaml:"redis"`
	Auth     AuthConfig     `yaml:"auth"`
	Security SecurityConfig `yaml:"security"`
	Log      LogConfig      `yaml:"log"`
	AI       AIConfig       `yaml:"ai"`

	// Warnings holds non-fatal issues found while loading (e.g. generated
	// ephemeral secrets in dev mode). Not read from YAML.
	Warnings []string `yaml:"-"`
}

type ServerConfig struct {
	// Listen is the HTTP listen address, e.g. ":8080".
	Listen string `yaml:"listen"`
	// Mode is "prod" or "dev". In dev mode missing secrets are generated
	// (ephemeral) instead of failing startup.
	Mode string `yaml:"mode"`
	// StaticDir is the directory holding the SPA (public/). Empty disables static serving.
	StaticDir string `yaml:"static_dir"`
	// CORSOrigins lists allowed origins; "*" allows any (credentials are then not allowed).
	CORSOrigins     []string      `yaml:"cors_origins"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// TrustedProxies is passed to gin.SetTrustedProxies (for client IP extraction).
	TrustedProxies []string `yaml:"trusted_proxies"`
}

type DatabaseConfig struct {
	DSN         string `yaml:"dsn"`
	MaxConns    int32  `yaml:"max_conns"`
	AutoMigrate bool   `yaml:"auto_migrate"`
}

// RedisConfig is reserved for session cache / distributed locks used by later modules.
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type AuthConfig struct {
	JWTSecret            string        `yaml:"jwt_secret"`
	Issuer               string        `yaml:"issuer"`
	AccessTTL            time.Duration `yaml:"access_ttl"`
	RefreshTTL           time.Duration `yaml:"refresh_ttl"`
	MaxLoginFailures     int           `yaml:"max_login_failures"`
	LockoutDuration      time.Duration `yaml:"lockout_duration"`
	LoginRatePerMinute   int           `yaml:"login_rate_per_minute"`
	InitialAdminPassword string        `yaml:"initial_admin_password"`
}

type SecurityConfig struct {
	// EncryptionKey is a 32-byte AES-256 key, base64 (std) or hex encoded.
	EncryptionKey string `yaml:"encryption_key"`
}

type LogConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // json|text
}

type AIConfig struct {
	RequestTimeout time.Duration `yaml:"request_timeout"`
	MaxToolRounds  int           `yaml:"max_tool_rounds"`
}

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:          ":8080",
			Mode:            "prod",
			StaticDir:       "./public",
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    0, // SSE streams must not be cut by a write deadline
			ShutdownTimeout: 15 * time.Second,
		},
		Database: DatabaseConfig{
			DSN:         "postgres://matrix:matrix@127.0.0.1:5432/matrix?sslmode=disable",
			MaxConns:    20,
			AutoMigrate: true,
		},
		Redis: RedisConfig{Addr: "127.0.0.1:6379"},
		Auth: AuthConfig{
			Issuer:               "xingshu-matrix",
			AccessTTL:            15 * time.Minute,
			RefreshTTL:           7 * 24 * time.Hour,
			MaxLoginFailures:     5,
			LockoutDuration:      15 * time.Minute,
			LoginRatePerMinute:   20,
			InitialAdminPassword: "Xingshu@2026",
		},
		Log: LogConfig{Level: "info", Format: "json"},
		AI:  AIConfig{RequestTimeout: 120 * time.Second, MaxToolRounds: 5},
	}
}

// Load reads the YAML file at path (optional; "" skips) then applies env overrides
// and validates.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if err := applyEnv(cfg, os.LookupEnv); err != nil {
		return nil, err
	}
	if err := cfg.finalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

type lookupFunc func(string) (string, bool)

func applyEnv(c *Config, env lookupFunc) error {
	str := func(key string, dst *string) {
		if v, ok := env(key); ok {
			*dst = v
		}
	}
	str("MATRIX_LISTEN", &c.Server.Listen)
	str("MATRIX_MODE", &c.Server.Mode)
	str("MATRIX_STATIC_DIR", &c.Server.StaticDir)
	str("MATRIX_DB_DSN", &c.Database.DSN)
	str("MATRIX_REDIS_ADDR", &c.Redis.Addr)
	str("MATRIX_REDIS_PASSWORD", &c.Redis.Password)
	str("MATRIX_JWT_SECRET", &c.Auth.JWTSecret)
	str("MATRIX_ADMIN_INITIAL_PASSWORD", &c.Auth.InitialAdminPassword)
	str("MATRIX_ENCRYPTION_KEY", &c.Security.EncryptionKey)
	str("MATRIX_LOG_LEVEL", &c.Log.Level)
	str("MATRIX_LOG_FORMAT", &c.Log.Format)
	if v, ok := env("MATRIX_CORS_ORIGINS"); ok {
		c.Server.CORSOrigins = splitList(v)
	}
	if v, ok := env("MATRIX_TRUSTED_PROXIES"); ok {
		c.Server.TrustedProxies = splitList(v)
	}
	if v, ok := env("MATRIX_DB_AUTO_MIGRATE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("MATRIX_DB_AUTO_MIGRATE: %w", err)
		}
		c.Database.AutoMigrate = b
	}
	if v, ok := env("MATRIX_REDIS_DB"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("MATRIX_REDIS_DB: %w", err)
		}
		c.Redis.DB = n
	}
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsDev reports whether the server runs in dev mode.
func (c *Config) IsDev() bool { return c.Server.Mode == "dev" }

func (c *Config) finalize() error {
	c.Warnings = nil
	var errs []error
	if c.Server.Mode != "prod" && c.Server.Mode != "dev" {
		errs = append(errs, fmt.Errorf("server.mode must be prod or dev, got %q", c.Server.Mode))
	}
	if c.Server.Listen == "" {
		errs = append(errs, errors.New("server.listen is required"))
	}
	if c.Database.DSN == "" {
		errs = append(errs, errors.New("database.dsn is required"))
	}
	if c.Auth.JWTSecret == "" && c.IsDev() {
		c.Auth.JWTSecret = randomString(48)
		c.Warnings = append(c.Warnings, "auth.jwt_secret not set: generated an ephemeral secret (dev mode); tokens will not survive restarts")
	}
	if len(c.Auth.JWTSecret) < 32 {
		errs = append(errs, errors.New("auth.jwt_secret must be at least 32 characters (MATRIX_JWT_SECRET)"))
	}
	if c.Security.EncryptionKey == "" && c.IsDev() {
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		c.Security.EncryptionKey = base64.StdEncoding.EncodeToString(k)
		c.Warnings = append(c.Warnings, "security.encryption_key not set: generated an ephemeral key (dev mode); encrypted data will be unreadable after restart")
	}
	if _, err := c.EncryptionKeyBytes(); err != nil {
		errs = append(errs, err)
	}
	if c.Auth.AccessTTL <= 0 || c.Auth.RefreshTTL <= c.Auth.AccessTTL {
		errs = append(errs, errors.New("auth.access_ttl must be > 0 and refresh_ttl > access_ttl"))
	}
	if c.Auth.MaxLoginFailures <= 0 {
		c.Auth.MaxLoginFailures = 5
	}
	if c.Auth.LockoutDuration <= 0 {
		c.Auth.LockoutDuration = 15 * time.Minute
	}
	if c.Auth.LoginRatePerMinute <= 0 {
		c.Auth.LoginRatePerMinute = 20
	}
	if c.Auth.InitialAdminPassword == "" {
		c.Auth.InitialAdminPassword = "Xingshu@2026"
	}
	if c.AI.MaxToolRounds <= 0 {
		c.AI.MaxToolRounds = 5
	}
	if c.AI.RequestTimeout <= 0 {
		c.AI.RequestTimeout = 120 * time.Second
	}
	return errors.Join(errs...)
}

// EncryptionKeyBytes decodes security.encryption_key into 32 raw bytes.
func (c *Config) EncryptionKeyBytes() ([]byte, error) {
	k := strings.TrimSpace(c.Security.EncryptionKey)
	if k == "" {
		return nil, errors.New("security.encryption_key is required (32 bytes, base64 or hex; MATRIX_ENCRYPTION_KEY)")
	}
	if b, err := hex.DecodeString(k); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(k); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errors.New("security.encryption_key must decode (base64 or hex) to exactly 32 bytes")
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}
