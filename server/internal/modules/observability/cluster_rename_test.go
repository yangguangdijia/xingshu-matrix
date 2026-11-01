package observability

import (
	"errors"
	"net/http"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

func TestRenameCentralValueMovesNameKeyAndKeepsLabelValue(t *testing.T) {
	got, err := renameCentralValue(map[string]any{"prod-a": "east-1", "9": "west-1"}, "7", "prod-a", "prod-east")
	if err != nil {
		t.Fatal(err)
	}
	if got["prod-east"] != "east-1" || got["9"] != "west-1" {
		t.Fatalf("unexpected mapping %v", got)
	}
	if _, stale := got["prod-a"]; stale {
		t.Fatalf("old name key survived: %v", got)
	}
}

func TestRenameCentralValueIgnoresMappingsWithoutOldName(t *testing.T) {
	in := map[string]any{"7": "east-1"}
	got, err := renameCentralValue(in, "7", "prod-a", "prod-east")
	if err != nil || len(got) != 1 || got["7"] != "east-1" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestRenameCentralValueRejectsAmbiguousMappings(t *testing.T) {
	cases := map[string]map[string]any{
		"new name already mapped elsewhere": {"prod-a": "east-1", "prod-east": "other"},
		"id mapping disagrees":              {"prod-a": "east-1", "7": "other"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := renameCentralValue(values, "7", "prod-a", "prod-east")
			var httpErr *httpx.Error
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict {
				t.Fatalf("want 409, got %v", err)
			}
		})
	}
}
