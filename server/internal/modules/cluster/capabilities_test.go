package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func capabilityRESTTestClients(t *testing.T, handler http.HandlerFunc) *kubeClients {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	config := &rest.Config{Host: server.URL}
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return &kubeClients{cs: cs, dyn: dyn}
}

func capabilityTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestCapabilityResourceFollowsContinuationAndPreservesPartialFailure(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "services"}
	for _, laterForbidden := range []bool{false, true} {
		t.Run(fmt.Sprintf("laterForbidden=%v", laterForbidden), func(t *testing.T) {
			var continuations []string
			kc := capabilityRESTTestClients(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1":
					capabilityTestJSON(t, w, metav1.APIResourceList{
						GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "services", Kind: "Service", Namespaced: true, Verbs: []string{"list"}}},
					})
				case "/api/v1/services":
					if r.URL.Query().Get("limit") != "500" {
						t.Errorf("page limit = %q", r.URL.Query().Get("limit"))
					}
					token := r.URL.Query().Get("continue")
					continuations = append(continuations, token)
					if token != "" && laterForbidden {
						w.WriteHeader(http.StatusForbidden)
						capabilityTestJSON(t, w, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
							Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403,
							Message: "Authorization: Bearer never-return-this"})
						return
					}
					items := make([]corev1.Service, capabilityListLimit)
					next, start := "page-2", 0
					if token != "" {
						items, next, start = items[:1], "", capabilityListLimit
					}
					for i := range items {
						items[i].ObjectMeta = metav1.ObjectMeta{Name: fmt.Sprintf("service-%04d", start+i), Namespace: "apps"}
					}
					capabilityTestJSON(t, w, corev1.ServiceList{TypeMeta: metav1.TypeMeta{Kind: "ServiceList", APIVersion: "v1"},
						ListMeta: metav1.ListMeta{Continue: next}, Items: items})
				default:
					http.NotFound(w, r)
				}
			})
			got := capabilityResource(context.Background(), kc, gvr, "")
			if len(continuations) != 2 || continuations[0] != "" || continuations[1] != "page-2" {
				t.Fatalf("continuations = %v", continuations)
			}
			if laterForbidden {
				if got.Available || got.State != "forbidden" || !got.Truncated || len(got.Items) != 500 ||
					strings.Contains(got.Error, "never-return-this") {
					t.Fatalf("partial failure = %+v", got)
				}
			} else if !got.Available || got.State != "available" || got.Truncated || len(got.Items) != 501 {
				t.Fatalf("completed inventory = %+v", got)
			}
		})
	}
}

func TestCapabilityPagesBoundsAndCancellation(t *testing.T) {
	tests := []struct {
		name      string
		pageItems int
		next      func(int) string
		wantCalls int
		wantItems int
		wantErr   error
	}{
		{"repeated", 1, func(int) string { return "same" }, 2, 2, errCapabilityContinue},
		{"cycle", 1, func(i int) string {
			if i%2 == 0 {
				return "second"
			}
			return "first"
		}, 3, 3, errCapabilityContinue},
		{"page-cap", 1, func(i int) string { return strconv.Itoa(i) }, capabilityMaxPages, capabilityMaxPages, errCapabilityTruncated},
		{"item-cap", capabilityMaxItems + 1, func(int) string { return "" }, 1, capabilityMaxItems, errCapabilityTruncated},
		{"exact-cap-complete", capabilityMaxItems, func(int) string { return "" }, 1, capabilityMaxItems, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			items, err := capabilityPages(context.Background(), metav1.ListOptions{Limit: 99999}, func(ctx context.Context, opts metav1.ListOptions) ([]int, string, error) {
				calls++
				if opts.Limit != capabilityListLimit {
					t.Fatalf("unbounded limit %d", opts.Limit)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > capabilityListTimeout {
					t.Fatal("missing bounded deadline")
				}
				return make([]int, tt.pageItems), tt.next(calls), nil
			})
			if !errors.Is(err, tt.wantErr) || len(items) != tt.wantItems || calls != tt.wantCalls {
				t.Fatalf("items=%d calls=%d err=%v", len(items), calls, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := capabilityPages(ctx, metav1.ListOptions{}, func(context.Context, metav1.ListOptions) ([]int, string, error) {
		t.Fatal("requested a page after cancellation")
		return nil, "", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestTypedCapabilityPaginationUsesRESTContinuation(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet", "Pod"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			kc := capabilityRESTTestClients(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				token := r.URL.Query().Get("continue")
				if calls == 1 && token != "" || calls == 2 && token != "second" {
					t.Errorf("call %d continuation = %q", calls, token)
				}
				count, next := 500, "second"
				if calls == 2 {
					count, next = 1, ""
				}
				items := make([]map[string]any, count)
				for i := range items {
					items[i] = map[string]any{"metadata": map[string]any{"name": fmt.Sprintf("item-%d-%d", calls, i)}}
				}
				apiVersion := "apps/v1"
				if kind == "Pod" {
					apiVersion = "v1"
				}
				capabilityTestJSON(t, w, map[string]any{"kind": kind + "List", "apiVersion": apiVersion,
					"metadata": map[string]any{"continue": next}, "items": items})
			})
			var count int
			var err error
			switch kind {
			case "Deployment":
				var list *appsv1.DeploymentList
				list, err = listDeploymentsPaged(context.Background(), kc, metav1.ListOptions{})
				count = len(list.Items)
			case "StatefulSet":
				var list *appsv1.StatefulSetList
				list, err = listStatefulSetsPaged(context.Background(), kc, metav1.ListOptions{})
				count = len(list.Items)
			case "DaemonSet":
				var list *appsv1.DaemonSetList
				list, err = listDaemonSetsPaged(context.Background(), kc, metav1.ListOptions{})
				count = len(list.Items)
			case "Pod":
				var list *corev1.PodList
				list, err = listPodsPaged(context.Background(), kc, metav1.ListOptions{})
				count = len(list.Items)
			}
			if err != nil || count != 501 || calls != 2 {
				t.Fatalf("count=%d calls=%d err=%v", count, calls, err)
			}
		})
	}
}

func TestTypedCapabilityPaginationReturnsPartialListAndError(t *testing.T) {
	calls := 0
	kc := capabilityRESTTestClients(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			capabilityTestJSON(t, w, appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{Kind: "DeploymentList", APIVersion: "apps/v1"},
				ListMeta: metav1.ListMeta{Continue: "second"}, Items: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "first"}}}})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		capabilityTestJSON(t, w, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403})
	})
	list, err := listDeploymentsPaged(context.Background(), kc, metav1.ListOptions{})
	if !apierrors.IsForbidden(err) || list == nil || len(list.Items) != 1 {
		t.Fatalf("partial list=%+v err=%v", list, err)
	}
}

func TestComponentCapabilitySeparatesRegisteredAPIAndReadyController(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	cs.Discovery().(*fake.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: "cert-manager.io/v1", APIResources: []metav1.APIResource{{Name: "certificates", Kind: "Certificate", Verbs: []string{"list"}}},
	}}
	kc := &kubeClients{cs: cs}
	probe := componentProbes["cert_manager"]
	apiOnly := componentCapability(kc, "cert_manager", probe, workloadInventory{})
	if apiOnly.Available || apiOnly.State != "partial" || len(apiOnly.Items) != 1 ||
		apiOnly.Items[0].Source != "api" || apiOnly.Items[0].State != "registered" || apiOnly.Items[0].Readiness != "unknown" {
		t.Fatalf("registered API treated as controller: %+v", apiOnly)
	}
	item := capabilityWorkload(metav1.ObjectMeta{Name: "release-controller", Namespace: "cert-manager",
		Labels: map[string]string{"app.kubernetes.io/name": "cert-manager"}}, "Deployment", "not_ready")
	notReady := componentCapability(kc, "cert_manager", probe, workloadInventory{items: []CapabilityItem{item}})
	if notReady.Available || notReady.State != "partial" {
		t.Fatalf("unready controller = %+v", notReady)
	}
	item.Readiness = "ready"
	ready := componentCapability(kc, "cert_manager", probe, workloadInventory{items: []CapabilityItem{item}})
	if !ready.Available || ready.State != "available" {
		t.Fatalf("ready controller = %+v", ready)
	}
	incomplete := componentCapability(kc, "cert_manager", probe, workloadInventory{items: []CapabilityItem{item},
		err: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("secret-token"))})
	if incomplete.Available || incomplete.State != "forbidden" || !incomplete.Truncated || strings.Contains(incomplete.Error, "secret-token") {
		t.Fatalf("incomplete evidence = %+v", incomplete)
	}
}

func TestComponentCapabilityRejectsSubstringAndOperatorIdentity(t *testing.T) {
	kc := &kubeClients{cs: kubefake.NewSimpleClientset()}
	for _, item := range []CapabilityItem{
		{Name: "not-jenkins-application", Kind: "Deployment", Readiness: "ready"},
		{Name: "operator", Kind: "Deployment", Readiness: "ready", Labels: map[string]string{"app.kubernetes.io/name": "jenkins-operator"}},
		{Name: "tenant", Kind: "Deployment", Readiness: "ready", Labels: map[string]string{"app.kubernetes.io/instance": "jenkins"}},
		{Name: "operator", Kind: "Deployment", Readiness: "ready", Labels: map[string]string{"app.kubernetes.io/name": "jenkins-operator", "app.kubernetes.io/part-of": "jenkins"}},
	} {
		got := componentCapability(kc, "jenkins", componentProbes["jenkins"], workloadInventory{items: []CapabilityItem{item}})
		if got.Available || got.State != "missing" || len(got.Items) != 0 {
			t.Fatalf("misleading identity was accepted: %+v", got)
		}
	}
}

func TestComponentCapabilityPreservesForbiddenDuringPartialDiscovery(t *testing.T) {
	kc := capabilityRESTTestClients(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis":
			capabilityTestJSON(t, w, metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "jenkins.io",
				Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "jenkins.io/v1", Version: "v1"}, {GroupVersion: "jenkins.io/v2", Version: "v2"}}}}})
		case "/apis/jenkins.io/v1":
			capabilityTestJSON(t, w, metav1.APIResourceList{GroupVersion: "jenkins.io/v1",
				APIResources: []metav1.APIResource{{Name: "jenkins", Kind: "Jenkins", Verbs: []string{"list"}}}})
		default:
			w.WriteHeader(http.StatusForbidden)
			capabilityTestJSON(t, w, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
				Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403, Message: "secret-token"})
		}
	})
	got := componentCapability(kc, "jenkins", componentProbes["jenkins"], workloadInventory{})
	if got.Available || got.State != "forbidden" || !got.Truncated || len(got.Items) != 1 || strings.Contains(got.Error, "secret-token") {
		t.Fatalf("partially forbidden discovery = %+v", got)
	}
}

func TestVeleroCapabilityPreservesIncompleteAndForbiddenStates(t *testing.T) {
	got := veleroCapability(map[string]ClusterCapability{
		"velero_backups":           {State: "partial", Truncated: true},
		"velero_schedules":         {State: "forbidden"},
		"velero_storage_locations": {State: "missing"},
	})
	if got.Available || got.State != "forbidden" || !got.Truncated {
		t.Fatalf("Velero hid partial/forbidden state: %+v", got)
	}
}

func TestComponentWorkloadsPreserveDaemonSetKindAndReadiness(t *testing.T) {
	cs := kubefake.NewSimpleClientset(&appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "mesh", Generation: 2, Labels: map[string]string{"app": "istio"}},
		Status:     appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 3, NumberReady: 3, UpdatedNumberScheduled: 3},
	})
	inventory := listComponentWorkloads(context.Background(), &kubeClients{cs: cs})
	if inventory.err != nil || len(inventory.items) != 1 || inventory.items[0].Kind != "DaemonSet" ||
		inventory.items[0].Source != "node-workload" || inventory.items[0].Readiness != "ready" {
		t.Fatalf("daemonset inventory = %+v", inventory)
	}
}

func TestCapabilityErrorsNeverExposeUpstreamMessages(t *testing.T) {
	for _, err := range []error{
		errors.New("https://username:password@example.test?token=secret-token"),
		apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "secret-token", errors.New("Authorization: Bearer secret-token")),
		apierrors.NewNotFound(schema.GroupResource{Resource: "secret-token"}, "secret-token"),
	} {
		_, message := classifyCapabilityError(err)
		if strings.Contains(message, "secret-token") || strings.Contains(message, "password") || strings.Contains(message, "Bearer") {
			t.Fatalf("upstream error exposed: %q", message)
		}
	}
}
