package cluster

import (
	"strings"
	"testing"
)

func TestNormalizeClusterName(t *testing.T) {
	got, err := normalizeClusterName("  prod-east 生产  ")
	if err != nil || got != "prod-east 生产" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "   ", "a/b", `a\b`, "line\nbreak", "tab\tname", strings.Repeat("集", 129)} {
		if _, err := normalizeClusterName(bad); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
	if _, err := normalizeClusterName(strings.Repeat("集", 128)); err != nil {
		t.Fatalf("128 runes should be accepted: %v", err)
	}
}
