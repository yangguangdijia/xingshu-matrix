package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestIntegrationTypesAndMarkerBoundaries(t *testing.T) {
	types, err := parseIntegrationTypes("loki,prometheus,loki")
	if err != nil || !reflect.DeepEqual(types, []string{"loki", "prometheus"}) {
		t.Fatalf("types not validated/deduplicated: %v %v", types, err)
	}
	if additional, err := parseIntegrationTypes("alertmanager,elasticsearch"); err != nil ||
		!reflect.DeepEqual(additional, []string{"alertmanager", "elasticsearch"}) {
		t.Fatalf("query component types rejected: %v %v", additional, err)
	}
	for _, invalid := range []string{"loki,", "secret", "loki,fluentbit"} {
		if _, err := parseIntegrationTypes(invalid); err == nil {
			t.Fatalf("invalid types accepted: %s", invalid)
		}
	}
	for _, tc := range []struct {
		name, component string
		want            bool
	}{
		{"kube-prometheus-stack-prometheus", "prometheus", true},
		{"prometheus-operated", "prometheus", true},
		{"prometheus-node-exporter", "prometheus", false},
		{"prometheus-operator", "prometheus", false},
		{"notprometheus", "prometheus", false},
		{"loki-gateway", "loki", true},
		{"otel-tempo", "tempo", true},
		{"fluent-bit-x5wd", "fluent-bit", true},
		{"fluentbit", "fluent-bit", true},
		{"fluentd", "fluent-bit", false},
		{"alertmanager-main", "alertmanager", true},
		{"kube-prometheus-stack-alertmanager", "prometheus", false},
		{"elasticsearch-master", "elasticsearch", true},
		{"elasticsearch-exporter", "elasticsearch", false},
	} {
		if got := integrationMarker(tc.name, tc.component); got != tc.want {
			t.Errorf("%s/%s: got %t, want %t", tc.name, tc.component, got, tc.want)
		}
	}
	if integrationMatch("worker", nil, []string{"registry.example/prometheus/worker:latest"}, "prometheus") != "" {
		t.Fatal("registry path incorrectly counted as component identity")
	}
}

func TestIntegrationAdditionalQueryComponentsAndAddressScopes(t *testing.T) {
	for _, tc := range []struct {
		name, component string
		port            int32
	}{
		{"alertmanager-main", "alertmanager", 9093},
		{"elasticsearch-master", "elasticsearch", 9200},
	} {
		service := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "monitoring"},
			Spec:   corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: tc.port, NodePort: 30093}}},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.40"}}}}}
		items := identifyIntegrationCandidates(tc.component, 11, []corev1.Service{service}, nil)
		if len(items) != 1 || items[0].Key == "" || items[0].Host != tc.name+".monitoring.svc" ||
			items[0].ConnectionVerified || items[0].Reachability != "not_tested" {
			t.Fatalf("unexpected discovery: %+v", items)
		}
		foundInternal, foundLB := false, false
		for _, address := range items[0].Addresses {
			switch address.Scope {
			case "cluster_internal":
				foundInternal = address.Host == items[0].Host && address.Port == tc.port
			case "external_candidate":
				foundLB = address.Host == "192.0.2.40" && address.Port == tc.port
			default:
				t.Fatalf("unrecognized address scope: %+v", address)
			}
		}
		if !foundInternal || !foundLB || len(items[0].URLCandidates) != len(items[0].Addresses) {
			t.Fatalf("URLs and address metadata diverged: %+v", items[0])
		}
	}
	fluent := identifyIntegrationCandidates("fluent-bit", 11, []corev1.Service{{
		ObjectMeta: metav1.ObjectMeta{Name: "fluent-bit", Namespace: "logging"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}},
	}}, nil)
	if len(fluent) != 1 || !fluent[0].Collector || len(fluent[0].Addresses) != 0 || len(fluent[0].URLCandidates) != 0 {
		t.Fatalf("collector exposed a query endpoint: %+v", fluent)
	}
}

func TestIntegrationCandidatesIsolateSameEnvironmentClusters(t *testing.T) {
	service := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "loki", Namespace: "logging"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 3100}}}}
	a := readIntegrationCandidates(context.Background(), integrationTestClient(t, &service), 101, "", []string{"loki"})
	b := readIntegrationCandidates(context.Background(), integrationTestClient(t, &service), 102, "", []string{"loki"})
	if len(a.Items) != 1 || len(b.Items) != 1 || a.Items[0].Key == b.Items[0].Key ||
		a.Items[0].ClusterID != 101 || b.Items[0].ClusterID != 102 ||
		a.DiscoveredAt.IsZero() || b.Items[0].DiscoveredAt.IsZero() {
		t.Fatalf("discovery identity mixed across clusters: A=%+v B=%+v", a, b)
	}
}

func TestIntegrationCandidatesRespectNamespaceAndServiceSelectors(t *testing.T) {
	service := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "query", Namespace: "monitoring"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "metrics"},
			Ports:    []corev1.ServicePort{{Port: 9090, NodePort: 30090, Protocol: corev1.ProtocolTCP}},
		}}
	foreign := *service.DeepCopy()
	foreign.Namespace = "other"
	workloads := []integrationWorkload{
		{kind: "Deployment", name: "backend", namespace: "monitoring",
			labels: map[string]string{"app": "metrics"}, images: []string{"prom/prometheus:v3"}},
		{kind: "Pod", name: "backend-1", namespace: "monitoring",
			labels: map[string]string{"app": "metrics"}, images: []string{"prom/prometheus:v3"}, hostIP: "192.0.2.10"},
		{kind: "Pod", name: "unrelated", namespace: "other",
			labels: map[string]string{"app": "not-metrics"}, hostIP: "192.0.2.11"},
	}
	candidates := identifyIntegrationCandidates("prometheus", 42, []corev1.Service{service, foreign}, workloads)
	if len(candidates) != 1 || candidates[0].Namespace != "monitoring" || candidates[0].Service != "query" ||
		candidates[0].ClusterID != 42 || candidates[0].ConnectionVerified {
		t.Fatalf("unexpected candidate identity: %+v", candidates)
	}
	urls := strings.Join(candidates[0].URLCandidates, ",")
	if !strings.Contains(urls, "http://query.monitoring.svc:9090") ||
		!strings.Contains(urls, "http://192.0.2.10:30090") || strings.Contains(urls, "192.0.2.11") {
		t.Fatalf("incorrect query URLs: %s", urls)
	}
	// Empty selectors select no workloads, even if an unrelated Pod has
	// matching labels or the same name in another namespace.
	service.Spec.Selector = nil
	candidates = identifyIntegrationCandidates("prometheus", 42, []corev1.Service{service}, workloads)
	for _, candidate := range candidates {
		if candidate.Kind == "Service" {
			t.Fatalf("empty selector linked an unidentified Service: %+v", candidate)
		}
	}
}

func TestIntegrationQueryURLsExcludeIngestionAndUnsafeHosts(t *testing.T) {
	service := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "tempo", Namespace: "monitoring"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "http", Port: 3200, Protocol: corev1.ProtocolTCP},
			{Name: "grpc", Port: 9095, Protocol: corev1.ProtocolTCP},
			{Name: "otlp-http", Port: 4318, Protocol: corev1.ProtocolTCP},
			{Name: "metrics", Port: 80, Protocol: corev1.ProtocolTCP},
			{Name: "http", Port: 3200, Protocol: corev1.ProtocolUDP},
		}},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{
			{IP: "2001:db8::10"}, {Hostname: "tempo.example.test"}, {Hostname: "user:private@evil.test/path"},
		}}},
	}
	got := integrationServiceURLs(service, "tempo", nil)
	want := []string{"http://[2001:db8::10]:3200", "http://tempo.example.test:3200", "http://tempo.monitoring.svc:3200"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query URLs = %v, want %v", got, want)
	}
}

func integrationTestClient(t *testing.T, objects ...runtime.Object) *kubeClients {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &kubeClients{
		cs:  kubefake.NewSimpleClientset(objects...),
		dyn: dynamicfake.NewSimpleDynamicClient(scheme, objects...),
	}
}

func TestIntegrationInventoryFindsDaemonSetWithoutReturningConfiguration(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "collector", Namespace: "logging"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "collector"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "collector", Image: "cr.fluentbit.io/fluent/fluent-bit:3.1",
				Env: []corev1.EnvVar{{Name: "API_KEY", Value: "never-return-this"}},
			}}},
		}},
	}
	result := readIntegrationCandidates(context.Background(), integrationTestClient(t, ds), 7, "logging",
		[]string{"fluent-bit", "loki"})
	if !result.Components["fluent-bit"].Available || result.Components["loki"].State != "missing" ||
		len(result.Items) != 1 || result.Items[0].Kind != "DaemonSet" || len(result.Items[0].URLCandidates) != 0 {
		t.Fatalf("collector discovery failed: %+v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil || strings.Contains(string(raw), "API_KEY") || strings.Contains(string(raw), "never-return-this") ||
		strings.Contains(string(raw), "3.1") {
		t.Fatalf("configuration leaked into discovery response: %s %v", raw, err)
	}
	if result.Items[0].ConnectionVerified {
		t.Fatal("discovery must not assert connectivity")
	}
}

func TestIntegrationInventoryFailuresAreNotReportedMissing(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		err         error
	}{
		{"forbidden", "forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", errors.New("private-token"))},
		{"unavailable", "unavailable", apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, "")},
		{"transport", "error", errors.New("http://private-token@upstream")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kc := integrationTestClient(t)
			kc.cs.(*kubefake.Clientset).PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			out := readIntegrationCandidates(context.Background(), kc, 1, "", []string{"loki"})
			raw, _ := json.Marshal(out)
			if out.Components["loki"].State != tc.state || len(out.Items) != 0 || strings.Contains(string(raw), "private-token") {
				t.Fatalf("incorrect failure state: %s", raw)
			}
		})
	}
}

func TestIntegrationTruncatedInventoryCannotProveMissing(t *testing.T) {
	kc := integrationTestClient(t)
	kc.cs.(*kubefake.Clientset).PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.ServiceList{ListMeta: metav1.ListMeta{Continue: "more"}}, nil
	})
	out := readIntegrationCandidates(context.Background(), kc, 1, "", []string{"loki"})
	if !out.Truncated || out.Components["loki"].State != "unavailable" || !out.Components["loki"].Truncated {
		t.Fatalf("truncated inventory misrepresented: %+v", out)
	}
}

func TestIntegrationWorkloadOnlyDoesNotInventURL(t *testing.T) {
	candidates := identifyIntegrationCandidates("loki", 1, nil, []integrationWorkload{
		{kind: "StatefulSet", name: "loki", namespace: "logging"},
	})
	status := integrationComponentStatus(candidates, map[string]networkReadStatus{
		"services": {Available: true, State: "available"},
	}, false)
	if len(candidates) != 1 || len(candidates[0].URLCandidates) != 0 || candidates[0].State != "unavailable" ||
		status.Available || status.State != "unavailable" {
		t.Fatalf("workload evidence must not invent a query endpoint: %+v %+v", candidates, status)
	}
}
