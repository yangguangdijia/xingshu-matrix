package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic/fake"
	kubeFake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/restmapper"
	k8stesting "k8s.io/client-go/testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

func testGovernedMapper(t *testing.T, resources ...*metav1.APIResourceList) *restmapper.DeferredDiscoveryRESTMapper {
	t.Helper()
	cs := kubeFake.NewSimpleClientset()
	discovery := cs.Discovery().(*discoveryfake.FakeDiscovery)
	discovery.Resources = resources
	return restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discovery))
}

func TestGovernedYAMLAllowListsAndScopes(t *testing.T) {
	valid := `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: reader
  namespace: apps
rules: []
`
	if _, err := governedManifests(valid, "", false); err != nil {
		t.Fatalf("valid Role rejected: %v", err)
	}
	if _, err := governedManifests(`apiVersion: v1
kind: Secret
metadata:
  name: credentials
  namespace: apps
`, "", false); err == nil {
		t.Fatal("RBAC endpoint accepted Secret YAML")
	}
	if _, err := governedManifests(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: reader
  namespace: apps
`, "", false); err == nil {
		t.Fatal("RBAC endpoint accepted namespace on cluster-scoped object")
	}
}

func TestVeleroScheduleRequiresScheduleExpression(t *testing.T) {
	pl := changePayload{Action: "backup_apply", Params: json.RawMessage(`{"yaml":"apiVersion: velero.io/v1\nkind: Schedule\nmetadata:\n  name: nightly\n  namespace: velero\nspec: {}\n"}`)}
	if err := validateGovernedChange(&pl); err == nil || !strings.Contains(err.Error(), "spec.schedule") {
		t.Fatalf("expected required Velero schedule expression, got %v", err)
	}
}

func TestRBACDeleteRequiresKindScopeAndIdentity(t *testing.T) {
	valid := changePayload{Action: "rbac_delete", Kind: "ClusterRole", Namespace: "-",
		Name: "reader", Params: json.RawMessage(`{"uid":"uid-1","resource_version":"rv-1"}`)}
	if err := validateGovernedChange(&valid); err != nil {
		t.Fatalf("valid cluster RBAC delete rejected: %v", err)
	}
	if valid.Kind != "clusterroles" || valid.Namespace != "" {
		t.Fatalf("cluster scope not normalized: %+v", valid)
	}
	for _, bad := range []changePayload{
		{Action: "rbac_delete", Kind: "Secret", Namespace: "apps", Name: "x", Params: json.RawMessage(`{"uid":"u","resource_version":"1"}`)},
		{Action: "rbac_delete", Kind: "Role", Namespace: "-", Name: "reader", Params: json.RawMessage(`{"uid":"u","resource_version":"1"}`)},
		{Action: "rbac_delete", Kind: "Role", Namespace: "apps", Name: "reader", Params: json.RawMessage(`{"uid":"u"}`)},
	} {
		if err := validateGovernedChange(&bad); err == nil {
			t.Errorf("invalid RBAC delete accepted: %+v", bad)
		}
	}
}

func TestGovernedApplyRejectsStaleApprovalBeforeWrite(t *testing.T) {
	const roleYAML = `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: reader
  namespace: apps
  uid: role-uid
  resourceVersion: "12"
rules:
- apiGroups: [""]
  resources: [pods]
  verbs: [get]
`
	role := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": "reader", "namespace": "apps", "uid": "role-uid", "resourceVersion": "13"},
	}}
	dyn := fake.NewSimpleDynamicClient(runtime.NewScheme(), role)
	kc := &kubeClients{dyn: dyn, mapper: testGovernedMapper(t, &metav1.APIResourceList{
		GroupVersion: "rbac.authorization.k8s.io/v1",
		APIResources: []metav1.APIResource{{Name: "roles", Kind: "Role", Namespaced: true}},
	})}
	_, err := applyGovernedYAML(context.Background(), kc, governedYAMLParams{YAML: roleYAML},
		map[string]bool{"roles": true}, false)
	var platformErr *httpx.Error
	if !errors.As(err, &platformErr) || platformErr.Status != http.StatusConflict {
		t.Fatalf("stale approved role should conflict, got %v", err)
	}
	for _, action := range dyn.Actions() {
		if action.GetVerb() == "patch" || action.GetVerb() == "create" || action.GetVerb() == "update" {
			t.Fatalf("stale approval wrote RBAC state: %s %s", action.GetVerb(), action.GetResource())
		}
	}
}

func TestGovernedApplyUpdatesWithReviewedResourceVersion(t *testing.T) {
	const roleYAML = `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: reader
  namespace: apps
  uid: role-uid
  resourceVersion: "12"
rules:
- apiGroups: [""]
  resources: [pods]
  verbs: [get]
`
	role := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": "reader", "namespace": "apps", "uid": "role-uid", "resourceVersion": "12"},
	}}
	dyn := fake.NewSimpleDynamicClient(runtime.NewScheme(), role)
	kc := &kubeClients{dyn: dyn, mapper: testGovernedMapper(t, &metav1.APIResourceList{
		GroupVersion: "rbac.authorization.k8s.io/v1",
		APIResources: []metav1.APIResource{{Name: "roles", Kind: "Role", Namespaced: true}},
	})}
	if _, err := applyGovernedYAML(context.Background(), kc, governedYAMLParams{YAML: roleYAML},
		map[string]bool{"roles": true}, false); err != nil {
		t.Fatalf("approved role update failed: %v", err)
	}
	updates := 0
	for _, action := range dyn.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatal("governed update used server-side apply without an atomic resourceVersion precondition")
		}
		if update, ok := action.(k8stesting.UpdateAction); ok {
			updated := update.GetObject().(*unstructured.Unstructured)
			if updated.GetUID() != "role-uid" || updated.GetResourceVersion() != "12" {
				t.Fatalf("update omitted reviewed identity: uid=%q rv=%q", updated.GetUID(), updated.GetResourceVersion())
			}
			updates++
		}
	}
	if updates != 2 { // One dry-run and one real update.
		t.Fatalf("expected dry-run and real optimistic-lock updates, got %d", updates)
	}
}

func TestClusterScopedRBACDeleteUsesClusterResourceInterface(t *testing.T) {
	dyn := fake.NewSimpleDynamicClient(runtime.NewScheme())
	var deleteAction k8stesting.Action
	dyn.PrependReactor("delete", "clusterroles", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction = action
		return true, nil, nil
	})
	_, err := executeGovernedResourceChange(context.Background(), &kubeClients{dyn: dyn}, changePayload{
		Action: "rbac_delete", Kind: "ClusterRole", Namespace: "-", Name: "reader",
		Params: json.RawMessage(`{"uid":"role-uid","resource_version":"12"}`),
	})
	if err != nil {
		t.Fatalf("cluster-scoped RBAC delete failed: %v", err)
	}
	if deleteAction == nil || deleteAction.GetNamespace() != "" {
		t.Fatalf("cluster-scoped delete used wrong namespace: %#v", deleteAction)
	}
}

func TestCapabilitiesDistinguishMissingAndForbidden(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingressclasses"}
	cs := kubeFake.NewSimpleClientset()
	dyn := fake.NewSimpleDynamicClient(runtime.NewScheme())
	missing := capabilityResource(context.Background(), &kubeClients{cs: cs, dyn: dyn}, gvr, "")
	if missing.Available || missing.State != "missing" {
		t.Fatalf("missing API state = %+v", missing)
	}

	cs.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "ingressclasses"}, "", nil)
	})
	forbidden := capabilityResource(context.Background(), &kubeClients{cs: cs, dyn: dyn}, gvr, "")
	if forbidden.Available || forbidden.State != "forbidden" {
		t.Fatalf("forbidden API state = %+v", forbidden)
	}
}

func TestVeleroApplyReturnsConflictWhenAPIsAreUnavailable(t *testing.T) {
	cs := kubeFake.NewSimpleClientset()
	kc := &kubeClients{cs: cs, dyn: fake.NewSimpleDynamicClient(runtime.NewScheme())}
	_, err := executeGovernedResourceChange(context.Background(), kc, changePayload{
		Action: "backup_apply",
		Params: json.RawMessage(`{"yaml":"apiVersion: velero.io/v1\nkind: Backup\nmetadata:\n  name: manual\n  namespace: velero\nspec: {}\n"}`),
	})
	var platformErr *httpx.Error
	if err == nil || !errors.As(err, &platformErr) || platformErr.Status != http.StatusConflict {
		t.Fatalf("expected explicit unavailable/conflict error, got %v", err)
	}
}

func TestSnapshotCapabilityMissingAPIStaysUnsupported(t *testing.T) {
	capability := snapshotCapability(context.Background(), &kubeClients{cs: kubeFake.NewSimpleClientset()})
	if capability.Supported || !strings.Contains(capability.Reason, "未安装") {
		t.Fatalf("missing snapshot CRD must remain unsupported: %+v", capability)
	}
}

func TestSnapshotRowPreservesSnapshotDetails(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]any{
			"name":              "daily-0",
			"namespace":         "apps",
			"uid":               "snapshot-uid",
			"resourceVersion":   "17",
			"labels":            map[string]any{"tier": "db"},
			"creationTimestamp": "2026-10-27T01:00:00Z",
			"managedFields":     []any{map[string]any{"manager": "test"}},
		},
		"spec": map[string]any{
			"volumeSnapshotClassName": "csi-class",
			"source":                  map[string]any{"persistentVolumeClaimName": "data"},
		},
		"status": map[string]any{
			"readyToUse":  true,
			"restoreSize": "10Gi",
		},
	}}
	row := snapshotRow(item)
	if row.Name != "daily-0" || row.Namespace != "apps" || row.Class != "csi-class" ||
		row.SourcePVC != "data" || !row.Ready || row.RestoreSize != "10Gi" ||
		row.UID != "snapshot-uid" || row.ResourceVersion != "17" || row.Labels["tier"] != "db" {
		t.Fatalf("snapshot details were not preserved: %+v", row)
	}
	if _, ok := item.Object["metadata"].(map[string]any)["managedFields"]; ok {
		t.Fatal("managedFields should be removed from snapshot output")
	}
}
