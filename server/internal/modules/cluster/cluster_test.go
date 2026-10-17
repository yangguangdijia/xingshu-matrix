package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func kubeconfigFor(server string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c1
  cluster: {server: %q, insecure-skip-tls-verify: true}
users:
- name: u1
  user: {token: abc}
contexts:
- name: ctx1
  context: {cluster: c1, user: u1}
current-context: ctx1
`, server)
}

func TestKubeconfigAndServerVersion(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" || r.Header.Get("Authorization") != "Bearer abc" {
			http.Error(w, "nope "+r.URL.Path+" "+r.Header.Get("Authorization"), 404)
			return
		}
		_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.4"}`))
	}))
	defer srv.Close()
	kc, server, err := normalizeKubeconfig(kubeconfigFor(srv.URL), "ctx1")
	if err != nil || server != srv.URL {
		t.Fatalf("normalize: %v %q", err, server)
	}
	if _, _, err := normalizeKubeconfig(kc, "missing"); err == nil {
		t.Fatal("expected missing context error")
	}
	cl, err := buildClients(kc, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := serverVersion(context.Background(), cl.cs)
	if err != nil || v != "v1.31.4" {
		t.Fatalf("version %q %v", v, err)
	}
}

func TestValidateChange(t *testing.T) {
	ok := changePayload{Action: "scale", Kind: "deploy", Params: json.RawMessage(`{"replicas":3}`)}
	if err := validateChange(&ok); err != nil || ok.Kind != "deployments" {
		t.Fatalf("scale: %v %s", err, ok.Kind)
	}
	for _, bad := range []changePayload{
		{Action: "scale", Kind: "deployments"},
		{Action: "scale", Kind: "daemonsets", Params: json.RawMessage(`{"replicas":1}`)},
		{Action: "restart", Kind: "pods"},
		{Action: "apply", Params: json.RawMessage(`{"yaml":"  "}`)},
		{Action: "rm -rf"},
	} {
		if err := validateChange(&bad); err == nil {
			t.Fatalf("expected error for %+v", bad)
		}
	}
}

func TestCordonLabelsAndDrain(t *testing.T) {
	ctrl := true
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"node-role.kubernetes.io/worker": ""}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	web := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web", Controller: &ctrl}}},
		Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	ds := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "kube-system",
		OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent", Controller: &ctrl}}},
		Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	cs := fake.NewSimpleClientset(node, web, ds, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}})
	var evicted []string
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := a.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		evicted = append(evicted, ev.Namespace+"/"+ev.Name)
		return true, nil, nil
	})
	kc := &kubeClients{cs: cs}
	ctx := context.Background()

	if _, err := executeOn(ctx, kc, changePayload{Action: "cordon", Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeOn(ctx, kc, changePayload{Action: "node_labels", Name: "n1", Params: json.RawMessage(`{"labels":{"zone":"a"}}`)}); err != nil {
		t.Fatal(err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if !n.Spec.Unschedulable || n.Labels["zone"] != "a" {
		t.Fatalf("node not patched: %+v", n)
	}
	row := nodeRow(*n)
	if row.Status != "Ready,SchedulingDisabled" || len(row.Roles) != 1 {
		t.Fatalf("row %+v", row)
	}
	res, err := executeOn(ctx, kc, changePayload{Action: "drain", Name: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["evicted"] != 1 || len(evicted) != 1 || evicted[0] != "default/web-1" {
		t.Fatalf("drain result %+v evicted=%v", m, evicted)
	}
	rows, n2, err := listWorkloadRows(ctx, cs, "deployments", "default", metav1.ListOptions{})
	if err != nil || n2 != 1 || rows.([]WorkloadRow)[0].Name != "web" {
		t.Fatalf("list deployments %v %v", rows, err)
	}
}

func TestStreamSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/logs", nil)
	streamSSE(c.Request.Context(), c, strings.NewReader("line1\nline2\n"))
	body := w.Body.String()
	if !strings.Contains(body, "event:log\ndata:line1") || !strings.Contains(body, "event:end") {
		t.Fatalf("sse body %q", body)
	}
}

func TestRoutesRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New().Register(r.Group("/api/v1"), &app.Deps{})
	if n := len(r.Routes()); n < 25 {
		t.Fatalf("routes %d", n)
	}
}
