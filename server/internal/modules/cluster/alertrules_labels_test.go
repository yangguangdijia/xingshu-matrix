package cluster

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func promCR(ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "monitoring.coreos.com/v1", "kind": "Prometheus",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     spec,
	}}
}

func TestPrometheusRuleLabelsFollowRuleSelector(t *testing.T) {
	listKinds := map[schema.GroupVersionResource]string{prometheusGVR: "PrometheusList"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds,
		promCR("monitoring", "kps", map[string]any{
			"ruleSelector":          map[string]any{"matchLabels": map[string]any{"release": "kps"}},
			"ruleNamespaceSelector": map[string]any{},
		}),
		// 未设置 ruleNamespaceSelector：只加载自身命名空间，shop-prod 的规则不应采用它的标签
		promCR("other", "local", map[string]any{
			"ruleSelector": map[string]any{"matchLabels": map[string]any{"team": "infra"}},
		}),
	)
	labels, loaders := prometheusRuleLabels(context.Background(), &kubeClients{dyn: dyn}, "shop-prod")
	if !reflect.DeepEqual(labels, map[string]string{"release": "kps"}) {
		t.Fatalf("labels = %v", labels)
	}
	if !reflect.DeepEqual(loaders, []string{"monitoring/kps"}) {
		t.Fatalf("loaders = %v", loaders)
	}
}

func TestPrometheusRuleLabelsWithoutOperator(t *testing.T) {
	listKinds := map[schema.GroupVersionResource]string{prometheusGVR: "PrometheusList"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	labels, loaders := prometheusRuleLabels(context.Background(), &kubeClients{dyn: dyn}, "default")
	if len(labels) != 0 || len(loaders) != 0 {
		t.Fatalf("expected no labels, got %v %v", labels, loaders)
	}
}
