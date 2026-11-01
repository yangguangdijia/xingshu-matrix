package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

func TestApplyYAMLReportsPartialExecution(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	patchCounts := map[string]int{}
	dyn.PrependReactor("patch", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		var body map[string]any
		if err := json.Unmarshal(patch.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		name := patch.GetName()
		patchCounts[name]++
		if patchCounts[name] == 1 {
			return true, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": name, "namespace": "apps"},
			}}, nil
		}
		if name == "second" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, name, errors.New("denied"))
		}
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": name, "namespace": "apps"},
		}}, nil
	})
	mapper := testGovernedMapper(t, &metav1.APIResourceList{
		GroupVersion: "apps/v1",
		APIResources: []metav1.APIResource{{Name: "deployments", Kind: "Deployment", Namespaced: true}},
	})
	kc := &kubeClients{dyn: dyn, mapper: mapper}
	yaml := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: first
  namespace: apps
spec:
  replicas: 1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: second
  namespace: apps
spec:
  replicas: 1
`
	value, err := applyYAML(context.Background(), kc, applyParams{YAML: yaml})
	var platformErr *httpx.Error
	if !errors.As(err, &platformErr) || platformErr.Status != http.StatusForbidden {
		t.Fatalf("expected apply failure after first write, got %v", err)
	}
	result, ok := value.(*applyBatchResult)
	if !ok {
		t.Fatalf("expected structured partial result, got %T", value)
	}
	if result.Applied != 1 || result.Failed != 1 || result.Skipped != 0 || result.Complete || result.State != "partial" {
		t.Fatalf("partial apply outcome is inaccurate: %+v", result)
	}
	if result.Objects[0].Status != "applied" || result.Objects[1].Status != "failed" {
		t.Fatalf("per-object apply statuses are inaccurate: %+v", result.Objects)
	}
}
