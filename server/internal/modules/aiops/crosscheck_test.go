package aiops

import (
	"context"
	"encoding/json"
	"testing"
)

// TestCrossAnalyzerParams checks that the parameter shapes the rule analyzers
// emit are accepted by the registered k8s actions' Validate.
func TestCrossAnalyzerParams(t *testing.T) {
	mj := analyzerMustJSON
	cases := []struct {
		action string
		params json.RawMessage
	}{
		{"k8s.logs", mj(map[string]any{"namespace": "a", "name": "p", "container": "c", "previous": true, "tail_lines": 200})},
		{"k8s.rollback_deployment", analyzerObjParams("deployment", "a", "d")},
		{"k8s.set_resources", mj(map[string]any{"kind": "deployment", "namespace": "a", "name": "d", "container": "c", "limits": map[string]string{"memory": "768Mi"}})},
		{"k8s.events", analyzerObjParams("pod", "a", "p")},
		{"k8s.events", mj(map[string]any{})},
		{"k8s.describe", analyzerObjParams("pod", "a", "p")},
		{"k8s.describe", analyzerObjParams("pvc", "a", "c")},
		{"k8s.describe", analyzerObjParams("deployment", "a", "d")},
		{"k8s.describe", analyzerObjParams("node", "", "n1")},
		{"k8s.cordon", analyzerObjParams("node", "", "n1")},
		{"k8s.drain", analyzerObjParams("node", "", "n1")},
		{"k8s.wait_ready", analyzerObjParams("deployment", "a", "d")},
	}
	for _, c := range cases {
		a, ok := ActionByName(c.action)
		if !ok {
			t.Fatalf("%s not registered", c.action)
		}
		if _, _, err := a.Validate(context.Background(), &ExecContext{}, c.params); err != nil {
			t.Errorf("%s %s: %v", c.action, c.params, err)
		}
	}
}
