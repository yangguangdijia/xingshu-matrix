package cicd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func callJSON(t *testing.T, h gin.HandlerFunc, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestPipelineYAMLRoundTrip(t *testing.T) {
	m := &Module{}
	code, out := callJSON(t, m.renderPipelineYAML, `{"name":"shop-api","runner_target_id":2,"on_push":true,
		"stages":[{"name":"unit","type":"test","image":"golang:1.23","commands":["go test ./..."]},{"name":"image","type":"build"}]}`)
	if code != http.StatusOK {
		t.Fatalf("render status %d: %v", code, out)
	}
	y, _ := out["data"].(map[string]any)["yaml"].(string)
	if !strings.Contains(y, "name: shop-api") || !strings.Contains(y, "go test ./...") {
		t.Fatalf("unexpected yaml:\n%s", y)
	}
	body, _ := json.Marshal(map[string]string{"yaml": y})
	code, out = callJSON(t, m.parsePipelineYAML, string(body))
	if code != http.StatusOK {
		t.Fatalf("parse status %d: %v", code, out)
	}
	doc := out["data"].(map[string]any)
	if doc["name"] != "shop-api" || len(doc["stages"].([]any)) != 2 || doc["on_push"] != true {
		t.Fatalf("round trip mismatch: %v", doc)
	}
}

func TestPipelineYAMLRejectsUnknownFields(t *testing.T) {
	m := &Module{}
	body, _ := json.Marshal(map[string]string{"yaml": "name: x\nstagez: []\n"})
	if code, _ := callJSON(t, m.parsePipelineYAML, string(body)); code == http.StatusOK {
		t.Fatal("unknown field accepted")
	}
	body, _ = json.Marshal(map[string]string{"yaml": "name: [unclosed\n"})
	if code, _ := callJSON(t, m.parsePipelineYAML, string(body)); code == http.StatusOK {
		t.Fatal("broken yaml accepted")
	}
}
