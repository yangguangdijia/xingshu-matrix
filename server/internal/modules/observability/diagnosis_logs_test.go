package observability

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestContainerDiagnosisLogsAreBoundedRedactedAndUsePreviousFallback(t *testing.T) {
	type request struct {
		container string
		previous  string
		tail      string
		limit     string
		timestamp string
	}
	var mu sync.Mutex
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		requests = append(requests, request{
			container: q.Get("container"), previous: q.Get("previous"),
			tail: q.Get("tailLines"), limit: q.Get("limitBytes"), timestamp: q.Get("timestamps"),
		})
		mu.Unlock()
		if q.Get("container") == "app" && q.Get("previous") == "true" {
			http.NotFound(w, r)
			return
		}
		if q.Get("container") == "app" {
			_, _ = fmt.Fprint(w, "ERROR password=private-value\n", strings.Repeat("a", 6000))
			return
		}
		_, _ = fmt.Fprint(w, strings.Repeat("b", 6000))
	}))
	defer server.Close()

	cs, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "prod"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "app", RestartCount: 3, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			{Name: "sidecar-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: "sidecar-2", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: "sidecar-3", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: "sidecar-4", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	}

	evidence := containerDiagnosisLogEvidence(context.Background(), cs, pod)
	mu.Lock()
	gotRequests := append([]request(nil), requests...)
	mu.Unlock()
	if len(evidence) != maxDiagnosisLogContainers {
		t.Fatalf("got %d log entries, want at most %d: %#v", len(evidence), maxDiagnosisLogContainers, evidence)
	}
	appLog := evidence[0]
	if appLog["container"] != "app" || appLog["previous"] != false || appLog["truncated"] != true {
		t.Fatalf("app logs did not fall back to bounded current logs: %#v", appLog)
	}
	content, _ := appLog["content"].(string)
	if !strings.Contains(content, "[REDACTED]") || strings.Contains(content, "private-value") {
		t.Fatalf("app log credential was not redacted: %q", content)
	}
	if evidence[1]["container"] != "sidecar-1" || evidence[3]["container"] != "sidecar-3" {
		t.Fatalf("unexpected container selection: %#v", evidence)
	}
	if evidence[3]["truncated"] != true || len(gotRequests) != maxDiagnosisLogContainers+1 {
		t.Fatalf("log byte/container bounds were not enforced: entries=%d requests=%d", len(evidence), len(gotRequests))
	}
	for _, got := range gotRequests {
		if got.tail != "80" || got.limit != "4096" || got.timestamp != "true" {
			t.Errorf("unbounded or incomplete Pod log request: %+v", got)
		}
	}
	if gotRequests[0].container != "app" || gotRequests[0].previous != "true" ||
		gotRequests[1].container != "app" || gotRequests[1].previous == "true" {
		t.Fatalf("previous log fallback requests were incorrect: %+v", gotRequests[:2])
	}
	for _, got := range gotRequests {
		if got.container == "sidecar-4" {
			t.Fatal("log collection exceeded the per-Pod container limit")
		}
	}
}
