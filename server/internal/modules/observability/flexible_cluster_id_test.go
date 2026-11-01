package observability

import (
	"encoding/json"
	"testing"
)

func TestFlexibleClusterIDAcceptsNumberAndNumericString(t *testing.T) {
	for _, input := range []string{`{"cluster_id":1}`, `{"cluster_id":"1"}`} {
		var payload struct {
			ClusterID flexibleClusterID `json:"cluster_id"`
		}
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			t.Fatalf("unmarshal %s: %v", input, err)
		}
		if string(payload.ClusterID) != "1" {
			t.Fatalf("cluster id from %s = %q, want 1", input, payload.ClusterID)
		}
	}
}

func TestFlexibleClusterIDRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{`{"cluster_id":0}`, `{"cluster_id":-1}`, `{"cluster_id":"abc"}`, `{"cluster_id":1.5}`} {
		var payload struct {
			ClusterID flexibleClusterID `json:"cluster_id"`
		}
		if err := json.Unmarshal([]byte(input), &payload); err == nil {
			t.Fatalf("invalid cluster id %s was accepted", input)
		}
	}
}
