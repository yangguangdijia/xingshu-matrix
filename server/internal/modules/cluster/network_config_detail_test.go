package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestNetworkDetailIdentityAllowlist(t *testing.T) {
	for _, identity := range []networkConfigurationIdentity{
		{"networking.k8s.io/v1", "Ingress", "apps", "web"},
		{"apisix.apache.org/v2beta3", "ApisixRoute", "apps", "web"},
		{"networking.k8s.io/v1", "IngressClass", "", "nginx"},
		{"networking.k8s.io/v1", "NetworkPolicy", "apps", "web"},
		{"cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", "web"},
	} {
		if _, err := resolveNetworkConfigurationIdentity(identity); err != nil {
			t.Errorf("valid identity rejected: %+v: %v", identity, err)
		}
	}
	for _, identity := range []networkConfigurationIdentity{
		{"v1", "Secret", "apps", "password"},
		{"other.example/v1", "Ingress", "apps", "web"},
		{"networking.k8s.io/v1", "Ingress", "", "web"},
		{"networking.k8s.io/v1", "IngressClass", "apps", "nginx"},
		{"networking.k8s.io/v1", "Ingress", "apps", "../web"},
		{"networking.k8s.io/../v1", "Ingress", "apps", "web"},
		{"v1", "ConfigMap", "apps", "kubeconfig"},
	} {
		if _, err := resolveNetworkConfigurationIdentity(identity); err == nil {
			t.Errorf("unsafe identity accepted: %+v", identity)
		}
	}
}

func networkDetailTestClient(objects ...runtime.Object) *kubeClients {
	cs := kubefake.NewSimpleClientset()
	cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: "networking.k8s.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "ingresses", Kind: "Ingress", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}},
			{Name: "ingressclasses", Kind: "IngressClass", Namespaced: false, Verbs: metav1.Verbs{"get"}},
		},
	}}
	return &kubeClients{cs: cs, dyn: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)}
}

func TestNetworkDetailReadsCurrentObjectByIdentity(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{"name": "web", "namespace": "apps", "resourceVersion": "17",
			"annotations":   map[string]any{"kubectl.kubernetes.io/last-applied-configuration": "historical-private-value", "custom": "keep"},
			"managedFields": []any{map[string]any{"manager": "kubectl"}}},
		"spec": map[string]any{"ingressClassName": "nginx", "rules": []any{map[string]any{"host": "example.test"}}},
	}}
	other := object.DeepCopy()
	other.SetNamespace("other")
	other.Object["spec"] = map[string]any{"ingressClassName": "wrong"}
	kc := networkDetailTestClient(object, other)
	identity := networkConfigurationIdentity{"networking.k8s.io/v1", "Ingress", "apps", "web"}
	resource, err := resolveNetworkConfigurationIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	result := readNetworkConfigurationDetail(context.Background(), kc, resource, identity)
	raw, err := json.Marshal(result)
	if err != nil || !result.Available || !strings.Contains(string(raw), "nginx") || strings.Contains(string(raw), "wrong") ||
		strings.Contains(string(raw), "historical-private-value") || strings.Contains(string(raw), "managedFields") {
		t.Fatalf("incomplete or unsafe detail: %s %v", raw, err)
	}
	if len(kc.dyn.(*dynamicfake.FakeDynamicClient).Actions()) != 1 ||
		kc.dyn.(*dynamicfake.FakeDynamicClient).Actions()[0].GetVerb() != "get" {
		t.Fatal("detail should issue exactly one read, without list or mutation")
	}
	if object.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"] == "" {
		t.Fatal("projection mutated the original object")
	}
}

func TestNetworkDetailFailureStates(t *testing.T) {
	identity := networkConfigurationIdentity{"networking.k8s.io/v1", "Ingress", "apps", "web"}
	resource, _ := resolveNetworkConfigurationIdentity(identity)
	for _, tc := range []struct {
		state string
		err   error
	}{
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "ingresses"}, "web", errors.New("private-token"))},
		{"missing", apierrors.NewNotFound(schema.GroupResource{Resource: "ingresses"}, "web")},
		{"error", errors.New("Authorization: private-token")},
	} {
		t.Run(tc.state, func(t *testing.T) {
			kc := networkDetailTestClient()
			kc.dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			out := readNetworkConfigurationDetail(context.Background(), kc, resource, identity)
			raw, _ := json.Marshal(out)
			if out.State != tc.state || out.Object != nil || out.Available || strings.Contains(string(raw), "private-token") {
				t.Fatalf("unsafe failure result: %s", raw)
			}
		})
	}
	kc := networkDetailTestClient()
	kc.cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = nil
	out := readNetworkConfigurationDetail(context.Background(), kc, resource, identity)
	if out.State != "unavailable" || out.Object != nil || len(kc.dyn.(*dynamicfake.FakeDynamicClient).Actions()) > 0 {
		t.Fatalf("discovery failure must prevent object read: %+v", out)
	}
}
