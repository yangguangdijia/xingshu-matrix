package cluster

import "testing"

type networkResourceKey struct {
	group, version, resource string
}

func TestExposeNetworkResourceCatalog(t *testing.T) {
	expected := []networkResource{
		{"networking.k8s.io", "v1", "ingresses", "Ingress", false},
		{"apisix.apache.org", "v2", "apisixroutes", "ApisixRoute", false},
	}

	got, ok := networkResources["expose"]
	if !ok {
		t.Fatal(`networkResources["expose"] is not registered`)
	}
	if len(got) != len(expected) {
		t.Fatalf("expose catalog has %d resources, want %d", len(got), len(expected))
	}

	expectedByGVR := make(map[networkResourceKey]networkResource, len(expected))
	for _, resource := range expected {
		expectedByGVR[networkResourceKey{resource.Group, resource.Version, resource.Resource}] = resource
	}

	seen := make(map[networkResourceKey]struct{}, len(got))
	for _, resource := range got {
		key := networkResourceKey{resource.Group, resource.Version, resource.Resource}
		if _, duplicate := seen[key]; duplicate {
			t.Errorf("expose catalog contains duplicate GVR %s/%s, resource %s", resource.Group, resource.Version, resource.Resource)
			continue
		}
		seen[key] = struct{}{}

		want, exists := expectedByGVR[key]
		if !exists {
			t.Errorf("unexpected expose resource: %+v", resource)
			continue
		}
		if resource != want {
			t.Errorf("resource %s/%s differs: got %+v, want %+v", resource.Group, resource.Version, resource, want)
		}
	}
}
