package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubeFake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

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

func TestNetworkConfigurationApplyAllowsDiscoveredIngressAndApisixRoute(t *testing.T) {
	resources := []*metav1.APIResourceList{
		{GroupVersion: "networking.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "ingresses", Kind: "Ingress", Namespaced: true}}},
		{GroupVersion: "apisix.apache.org/v2", APIResources: []metav1.APIResource{{Name: "apisixroutes", Kind: "ApisixRoute", Namespaced: true}}},
	}
	for _, manifest := range []string{
		"apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: web\n  namespace: apps\nspec: {}\n",
		"apiVersion: apisix.apache.org/v2\nkind: ApisixRoute\nmetadata:\n  name: web\n  namespace: apps\nspec: {}\n",
	} {
		t.Run(strings.Split(manifest, "\n")[1], func(t *testing.T) {
			cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}})
			cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = resources
			dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			dyn.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
				patch := action.(k8stesting.PatchAction).GetPatch()
				var object map[string]any
				if err := json.Unmarshal(patch, &object); err != nil {
					return true, nil, err
				}
				return true, &unstructured.Unstructured{Object: object}, nil
			})

			result, err := applyNetworkConfiguration(context.Background(), &kubeClients{cs: cs, dyn: dyn}, networkConfigurationApplyParams{YAML: manifest})
			if err != nil {
				t.Fatalf("apply rejected discovered resource: %v", err)
			}
			if result.(gin.H)["status"] != "applied" {
				t.Fatalf("unexpected apply result: %#v", result)
			}
			actions := dyn.Actions()
			if len(actions) != 1 || actions[0].GetVerb() != "patch" {
				t.Fatalf("expected one server-side apply patch, got %#v", actions)
			}
			patch := actions[0].(k8stesting.PatchAction)
			if patch.GetPatchType() != types.ApplyPatchType {
				t.Fatalf("expected server-side apply patch type, got %q", patch.GetPatchType())
			}
			wantResource := "ingresses"
			if strings.Contains(manifest, "ApisixRoute") {
				wantResource = "apisixroutes"
			}
			if patch.GetResource().Resource != wantResource || patch.GetNamespace() != "apps" {
				t.Fatalf("apply targeted %s/%s, want %s/apps", patch.GetResource().Resource, patch.GetNamespace(), wantResource)
			}
		})
	}
}

func TestNetworkConfigurationApplyRejectsOtherKinds(t *testing.T) {
	if permission := changePermission("network_configuration_apply"); permission != PermNetworkWrite {
		t.Fatalf("approval replay permission = %q, want %q", permission, PermNetworkWrite)
	}
	for _, manifest := range []string{
		"apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  namespace: apps\n",
		"apiVersion: networking.k8s.io/v1beta1\nkind: Ingress\nmetadata:\n  name: web\n  namespace: apps\n",
		"apiVersion: apisix.apache.org/v2\nkind: Secret\nmetadata:\n  name: web\n  namespace: apps\n",
	} {
		pl := changePayload{Action: "network_configuration_apply", Params: json.RawMessage(mustNetworkApplyJSON(t, manifest))}
		if err := validateChange(&pl); err == nil {
			t.Errorf("network configuration apply accepted disallowed manifest:\n%s", manifest)
		}
	}
}

func TestNetworkConfigurationApplyReportsUnavailableAPI(t *testing.T) {
	cs := kubeFake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	_, err := applyNetworkConfiguration(context.Background(), &kubeClients{cs: cs, dyn: dyn}, networkConfigurationApplyParams{
		YAML: "apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: web\n  namespace: apps\n",
	})
	var platformErr *httpx.Error
	if err == nil || !errors.As(err, &platformErr) || platformErr.Status != http.StatusConflict || !strings.Contains(platformErr.Message, "Ingress API networking.k8s.io/v1 is unavailable") {
		t.Fatalf("expected explicit unavailable API error, got %v", err)
	}

}

func mustNetworkApplyJSON(t *testing.T, manifest string) string {
	t.Helper()
	encoded, err := json.Marshal(networkConfigurationApplyParams{YAML: manifest})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
