package cluster

import (
	"reflect"
	"strings"
	"testing"
)

func TestComponentTierCoversCatalog(t *testing.T) {
	for _, def := range allComponentDefinitions() {
		info, ok := componentTiers[def.Key]
		if !ok {
			t.Fatalf("component %s has no tier", def.Key)
		}
		if info.Tier != ComponentTierRequired && info.Tier != ComponentTierRecommended && info.Tier != ComponentTierOptional {
			t.Fatalf("component %s has invalid tier %q", def.Key, info.Tier)
		}
		if strings.TrimSpace(info.TierReason) == "" {
			t.Fatalf("component %s has no tier reason", def.Key)
		}
	}
	for key := range componentTiers {
		if _, ok := componentDependencyGraph()[key]; !ok {
			t.Fatalf("tier entry %s has no catalog definition", key)
		}
	}
}

func TestComponentTierExtrasNotInstallable(t *testing.T) {
	for _, d := range extraComponentCatalog {
		if d.Installable || d.Supported {
			t.Fatalf("extra component %s must not be installable", d.Key)
		}
		if !strings.Contains(d.Notes, "待核实官方 chart 版本后开放安装") {
			t.Fatalf("extra component %s missing pending note", d.Key)
		}
		if _, ok := componentDefinition(d.Key); ok && d.Key != "" {
			// extras must not leak into the install-validation catalog
			t.Fatalf("extra component %s leaked into reviewed catalog", d.Key)
		}
	}
}

func TestComponentTierRealGraphAcyclic(t *testing.T) {
	var keys []string
	for key := range componentDependencyGraph() {
		keys = append(keys, key)
	}
	if _, _, _, err := planInstall(keys, nil); err != nil {
		t.Fatalf("real graph should be acyclic: %v", err)
	}
}

func TestComponentTierPlanOrder(t *testing.T) {
	order, missing, conflicts, err := planInstall([]string{"istio-gateway", "istiod", "istio-base"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"istio-base", "istiod", "istio-gateway"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if len(missing) != 0 || len(conflicts) != 0 {
		t.Fatalf("unexpected missing=%v conflicts=%v", missing, conflicts)
	}
}

func TestComponentTierPlanMissingDependencies(t *testing.T) {
	order, missing, _, err := planInstall([]string{"istio-gateway"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"istio-gateway"}) {
		t.Fatalf("order = %v", order)
	}
	if want := []string{"istio-base", "istiod"}; !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	_, missing, _, err = planInstall([]string{"istio-gateway"}, map[string]bool{"istio-base": true, "istiod": true})
	if err != nil || len(missing) != 0 {
		t.Fatalf("installed deps should satisfy plan: missing=%v err=%v", missing, err)
	}
	// transitive: istiod installed is not enough when istio-base is absent
	_, missing, _, _ = planInstall([]string{"istio-gateway"}, map[string]bool{"istiod": true})
	if !reflect.DeepEqual(missing, []string{"istio-base"}) {
		t.Fatalf("missing = %v", missing)
	}
}

func TestComponentTierPlanConflicts(t *testing.T) {
	_, _, conflicts, err := planInstall([]string{"kyverno", "gatekeeper"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{"gatekeeper", "kyverno"}}; !reflect.DeepEqual(conflicts, want) {
		t.Fatalf("conflicts = %v, want %v", conflicts, want)
	}
	_, _, conflicts, _ = planInstall([]string{"gatekeeper"}, map[string]bool{"kyverno": true})
	if len(conflicts) != 1 {
		t.Fatalf("conflict with installed component not detected: %v", conflicts)
	}
	_, _, conflicts, _ = planInstall([]string{"gatekeeper"}, map[string]bool{"kyverno": false})
	if len(conflicts) != 0 {
		t.Fatalf("installed=false must not conflict: %v", conflicts)
	}
}

func TestComponentTierPlanCycle(t *testing.T) {
	graph := map[string]componentGraphNode{
		"a": {DependsOn: []string{"b"}},
		"b": {DependsOn: []string{"c"}},
		"c": {DependsOn: []string{"a"}},
	}
	_, _, _, err := planInstallWithGraph(graph, []string{"a"}, nil)
	if err == nil || !strings.Contains(err.Error(), "依赖环") || !strings.Contains(err.Error(), "a -> b -> c -> a") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestComponentTierPlanErrors(t *testing.T) {
	if _, _, _, err := planInstall([]string{"nope"}, nil); err == nil || !strings.Contains(err.Error(), "未知组件") {
		t.Fatalf("unknown key err = %v", err)
	}
	if _, _, _, err := planInstall(nil, nil); err == nil {
		t.Fatal("empty request should fail")
	}
	graph := map[string]componentGraphNode{"a": {DependsOn: []string{"ghost"}}}
	if _, _, _, err := planInstallWithGraph(graph, []string{"a"}, nil); err == nil || !strings.Contains(err.Error(), "未登记") {
		t.Fatalf("unregistered dep err = %v", err)
	}
}
