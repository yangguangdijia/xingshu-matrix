package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicFake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testResourceCRD() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "widgets.example.com", "uid": "crd-uid", "resourceVersion": "12", "annotations": map[string]any{"last-applied": "private-marker"}},
		"spec": map[string]any{
			"group": "example.com", "scope": "Namespaced",
			"names":      map[string]any{"kind": "Widget", "plural": "widgets", "singular": "widget", "listKind": "WidgetList", "shortNames": []any{"wd"}},
			"conversion": map[string]any{"strategy": "Webhook", "webhook": map[string]any{"clientConfig": map[string]any{"caBundle": "private-marker", "url": "private-marker"}}},
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object", "required": []any{"spec"},
					"properties": map[string]any{"spec": map[string]any{
						"type": "object", "description": "private-marker", "default": map[string]any{"password": "private-marker"},
						"properties":               map[string]any{"mode": map[string]any{"type": "string", "enum": []any{"private-marker"}, "example": "private-marker"}},
						"x-kubernetes-validations": []any{map[string]any{"rule": "private-marker", "message": "private-marker"}},
					}},
				}},
			}},
		},
		"status": map[string]any{"storedVersions": []any{"v1"}, "conditions": []any{map[string]any{"type": "Established", "status": "True", "reason": "InitialNamesAccepted", "message": "private-marker"}}},
	}}
}

func TestResourceCRDProjectionShowsRealSchemaWithoutEmbeddedValues(t *testing.T) {
	item := testResourceCRD()
	row := resourceCRDView(*item, true)
	if row.Group != "example.com" || row.Kind != "Widget" || row.Plural != "widgets" || row.Scope != "Namespaced" ||
		row.ResourceVersion != "12" || row.ConversionStrategy != "Webhook" || len(row.Versions) != 1 || !row.Versions[0].Storage {
		t.Fatalf("CRD metadata projection incorrect: %+v", row)
	}
	if row.Versions[0].Schema["type"] != "object" || row.Versions[0].SchemaTruncated || len(row.Conditions) != 1 {
		t.Fatal("real schema and conditions were lost")
	}
	raw, _ := json.Marshal(row)
	if strings.Contains(string(raw), "private-marker") || strings.Contains(string(raw), "caBundle") || strings.Contains(string(raw), "annotations") {
		t.Fatal("raw CRD credentials or embedded values escaped projection")
	}
	inventory := resourceCRDView(*item, false)
	if inventory.Versions[0].Schema != nil {
		t.Fatal("inventory unexpectedly included schemas")
	}
	original, _ := json.Marshal(item)
	if !strings.Contains(string(original), "private-marker") {
		t.Fatal("projection modified the cached source")
	}
}

func TestResourceCRDSchemaProjectionBoundsDepthAndNodes(t *testing.T) {
	deep := map[string]any{"type": "string"}
	for i := 0; i < 50; i++ {
		deep = map[string]any{"items": deep}
	}
	budget := 10000
	if _, truncated := projectCRDSchema(deep, 0, &budget); !truncated {
		t.Fatal("schema depth cap ignored")
	}
	budget = 1
	if _, truncated := projectCRDSchema(map[string]any{"type": "object", "properties": map[string]any{"x": deep}}, 0, &budget); !truncated {
		t.Fatal("schema node cap ignored")
	}
}

func TestResourceCRDSchemaBudgetIgnoresPrivateFieldsAndBoundsMalformedChildren(t *testing.T) {
	input := map[string]any{"type": "object", "description": "private-marker"}
	for i := 0; i < 100; i++ {
		input[strings.Repeat("a", i+1)] = "private-marker"
	}
	budget := 2
	projected, truncated := projectCRDSchema(input, 0, &budget)
	if truncated || projected["type"] != "object" || len(projected) != 1 {
		t.Fatalf("private fields consumed the public schema budget: %+v %v", projected, truncated)
	}
	for _, input := range []map[string]any{
		{"properties": map[string]any{"a": "private-marker", "b": "private-marker", "c": "private-marker"}},
		{"allOf": []any{"private-marker", "private-marker", "private-marker"}},
	} {
		budget = 3
		projected, truncated = projectCRDSchema(input, 0, &budget)
		if !truncated || budget != 0 {
			t.Fatalf("malformed children bypassed the work budget: %+v %v %d", projected, truncated, budget)
		}
		raw, _ := json.Marshal(projected)
		if strings.Contains(string(raw), "private-marker") {
			t.Fatal("malformed child data escaped schema projection")
		}
	}
}

func TestResourceCRDInventoryDistinguishesMissingForbiddenAndFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status int
	}{
		{"absent", apierrors.NewNotFound(schema.GroupResource{Resource: "customresourcedefinitions"}, ""), 404},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, "", errors.New("private-marker")), 403},
		{"failed", errors.New("private-marker"), 502},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := dynamicFake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{resourceCRDGVR: "CustomResourceDefinitionList"})
			client.PrependReactor("list", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tt.err })
			_, _, err := readResourceCRDs(context.Background(), &kubeClients{dyn: client})
			var platform *httpx.Error
			if !errors.As(err, &platform) || platform.Status != tt.status || strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("incorrect sanitized error: %v", err)
			}
			if resourceReadState(tt.err) != tt.name {
				t.Fatalf("incorrect discovery state: %s", resourceReadState(tt.err))
			}
		})
	}
	client := dynamicFake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{resourceCRDGVR: "CustomResourceDefinitionList"}, testResourceCRD())
	rows, truncated, err := readResourceCRDs(context.Background(), &kubeClients{dyn: client})
	if err != nil || truncated || len(rows) != 1 {
		t.Fatalf("real CRD read failed: %+v %v %v", rows, truncated, err)
	}
}
