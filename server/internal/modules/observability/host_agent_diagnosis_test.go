package observability

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type diagnosticProvider struct {
	lookup app.HostDiagnosticLookup
	env    string
	ips    []string
}

type inspectionProvider struct {
	inspection app.HostDiagnosticInspection
	env        string
	ips        []string
}

func (p *diagnosticProvider) Snapshot(_ context.Context, env string, ips []string) (app.HostDiagnosticLookup, error) {
	p.env, p.ips = env, append([]string(nil), ips...)
	return p.lookup, nil
}

func (p *inspectionProvider) Inspect(_ context.Context, env string, ips []string) (app.HostDiagnosticInspection, error) {
	p.env, p.ips = env, append([]string(nil), ips...)
	return p.inspection, nil
}

func TestHostAgentDiagnosisEvidenceUsesFreshExactInternalIPAndBoundsFacts(t *testing.T) {
	now := time.Now()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}, Status: corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.8"},
			{Type: corev1.NodeExternalIP, Address: "203.0.113.8"},
		},
	}}
	disks := make([]app.HostDiagnosticDisk, 10)
	for i := range disks {
		disks[i] = app.HostDiagnosticDisk{Mount: "/disk", FSType: "ext4", Total: 100, Used: 80, Percent: 80}
	}
	provider := &diagnosticProvider{lookup: app.HostDiagnosticLookup{
		Status: "available",
		Snapshot: app.HostDiagnosticSnapshot{
			Name: "node password=private", IP: "10.0.0.8", AgentVersion: "1.2.3", LastSeenAt: now,
			OS: "linux", Platform: "ubuntu", PlatformVer: "24.04", Kernel: "6.8", Arch: "amd64",
			CPUCores: 8, CPUPercent: math.NaN(), Load1: 2.5, MemTotal: 1000, MemUsed: 700, MemPercent: 70,
			Uptime: 600, Disks: disks,
		},
	}}
	cs := fake.NewSimpleClientset(node)
	evidence := hostAgentDiagnosisEvidence(context.Background(), "prod", "worker-1", cs, provider)
	if evidence["status"] != "available" || provider.env != "prod" ||
		!reflect.DeepEqual(provider.ips, []string{"10.0.0.8"}) {
		t.Fatalf("diagnostics did not use same-env exact InternalIP: %#v, env=%q ips=%v", evidence, provider.env, provider.ips)
	}
	if evidence["cpu_percent"] != nil || len(evidence["disks"].([]map[string]any)) != 8 {
		t.Fatalf("invalid metrics or unbounded disk list: %#v", evidence)
	}
	raw, err := json.Marshal(evidence)
	if err != nil || strings.Contains(string(raw), "private") {
		t.Fatalf("host evidence was not JSON-safe or redacted: %s (%v)", raw, err)
	}
}

func TestHostAgentDiagnosisEvidenceDoesNotTrustUnavailableOrMismatchedSnapshots(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}, Status: corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.8"}},
	}}
	cs := fake.NewSimpleClientset(node)
	for _, status := range []string{"offline", "stale", "ambiguous", "not_registered", "unavailable"} {
		provider := &diagnosticProvider{lookup: app.HostDiagnosticLookup{Status: status}}
		got := hostAgentDiagnosisEvidence(context.Background(), "prod", "worker-1", cs, provider)
		if got["status"] != status {
			t.Errorf("status %q returned as %#v", status, got)
		}
	}
	provider := &diagnosticProvider{lookup: app.HostDiagnosticLookup{
		Status:   "available",
		Snapshot: app.HostDiagnosticSnapshot{IP: "10.0.0.80", LastSeenAt: time.Now()},
	}}
	if got := hostAgentDiagnosisEvidence(context.Background(), "prod", "worker-1", cs, provider); got["status"] != "stale" {
		t.Fatalf("similar but non-matching IP was trusted: %#v", got)
	}
}

func TestHostAgentInspectionEvidenceIsBoundedRedactedAndExactIPScoped(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}, Status: corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.8"},
			{Type: corev1.NodeExternalIP, Address: "203.0.113.8"}},
	}}
	provider := &inspectionProvider{inspection: app.HostDiagnosticInspection{
		Status: "available", CapturedAt: time.Now().UTC(),
		Output: "password=topsecret\n" + strings.Repeat("x", 9000),
	}}
	got := hostAgentInspectionEvidence(context.Background(), "prod", "worker-1", fake.NewSimpleClientset(node), provider)
	if got["status"] != "available" || got["read_only"] != true || provider.env != "prod" ||
		!reflect.DeepEqual(provider.ips, []string{"10.0.0.8"}) {
		t.Fatalf("inspection was not scoped to the exact InternalIP: %#v env=%q ips=%v", got, provider.env, provider.ips)
	}
	output, ok := got["output"].(string)
	if !ok || strings.Contains(output, "topsecret") || len([]rune(output)) > 8003 {
		t.Fatalf("inspection output was not redacted and bounded: len=%d output prefix=%q", len([]rune(output)), output[:min(len(output), 64)])
	}
	if strings.Contains(output, "password=") {
		t.Fatalf("credential key/value was not removed: %q", output[:min(len(output), 64)])
	}
}

func TestHostAgentInspectionEvidenceRejectsStaleAndUnsupportedResults(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}, Status: corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.8"}},
	}}
	cs := fake.NewSimpleClientset(node)
	for _, tc := range []struct {
		inspection app.HostDiagnosticInspection
		want       string
	}{
		{inspection: app.HostDiagnosticInspection{Status: "unsupported"}, want: "unsupported"},
		{inspection: app.HostDiagnosticInspection{Status: "available", CapturedAt: time.Now().Add(-3 * time.Minute), Output: "old"}, want: "stale"},
	} {
		got := hostAgentInspectionEvidence(context.Background(), "prod", "worker-1", cs, &inspectionProvider{inspection: tc.inspection})
		if got["status"] != tc.want {
			t.Errorf("inspection %#v returned %#v, want status %q", tc.inspection, got, tc.want)
		}
	}
}
