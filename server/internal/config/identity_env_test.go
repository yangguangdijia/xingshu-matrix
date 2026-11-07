package config

import (
	"strings"
	"testing"
)

func TestJWTPreviousSecretsEnv(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 32)
	cfg, err := loadWith(t, "", baseEnv(map[string]string{"MATRIX_JWT_PREVIOUS_SECRETS": " " + a + " ,," + b + " "}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth.JWTPreviousSecrets) != 2 || cfg.Auth.JWTPreviousSecrets[0] != a || cfg.Auth.JWTPreviousSecrets[1] != b {
		t.Fatalf("got %d previous secrets", len(cfg.Auth.JWTPreviousSecrets))
	}
	if _, err := loadWith(t, "", baseEnv(map[string]string{"MATRIX_JWT_PREVIOUS_SECRETS": "short"})); err == nil ||
		!strings.Contains(err.Error(), "jwt_previous_secrets") {
		t.Fatalf("short previous secret accepted: %v", err)
	}
	cfg, err = loadWith(t, "auth:\n  jwt_previous_secrets: [\""+a+"\"]\n", baseEnv(nil))
	if err != nil || len(cfg.Auth.JWTPreviousSecrets) != 1 {
		t.Fatalf("yaml: %v", err)
	}
	// 环境变量覆盖 YAML
	cfg, err = loadWith(t, "auth:\n  jwt_previous_secrets: [\""+a+"\"]\n", baseEnv(map[string]string{"MATRIX_JWT_PREVIOUS_SECRETS": ""}))
	if err != nil || len(cfg.Auth.JWTPreviousSecrets) != 0 {
		t.Fatalf("env override: %v", err)
	}
}
