package observability

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func TestHealthFindingsUseGracePeriodsAndLiveState(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-10 * time.Minute))
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod", UID: types.UID("pod-1"), CreationTimestamp: old},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "token=private"}},
		}}}}
	f := podFinding(p, now)
	if f == nil || f.Reason != "CrashLoopBackOff" || strings.Contains(f.Summary, "private") {
		t.Fatalf("expected redacted crash-loop finding: %+v", f)
	}
	p.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	if podFinding(p, now) != nil {
		t.Fatal("new Pod must receive startup grace")
	}
	p.CreationTimestamp = old
	p.DeletionTimestamp = &old
	if podFinding(p, now) != nil {
		t.Fatal("terminating Pod must not trigger repair")
	}
	p.DeletionTimestamp = nil
	p.Status.Phase = corev1.PodSucceeded
	if podFinding(p, now) != nil {
		t.Fatal("completed Pod must not be unhealthy")
	}
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", CreationTimestamp: old}}
	if got := nodeFindings(n, now); len(got) != 1 || got[0].Reason != "NodeNotReady" {
		t.Fatalf("missing node readiness alert: %+v", got)
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue}}
	if got := nodeFindings(n, now); len(got) != 1 || got[0].Reason != "DiskPressure" {
		t.Fatalf("pressure must remain observable on Ready nodes: %+v", got)
	}
}

func TestDeploymentHealthIgnoresScaledDownAndShortTransitions(t *testing.T) {
	now := time.Now()
	one, zero := int32(1), int32(0)
	d := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
		Spec: appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-3 * time.Minute)),
		}}}}
	if deploymentFinding(d, now) == nil {
		t.Fatal("sustained unavailability must be reported")
	}
	d.Spec.Replicas = &zero
	if deploymentFinding(d, now) != nil {
		t.Fatal("intentional scale-to-zero is not an outage")
	}
	d.Spec.Replicas = &one
	d.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-time.Minute))
	if deploymentFinding(d, now) != nil {
		t.Fatal("short availability transition must receive grace")
	}
}

func TestDiagnosisParsingRestrictsActionsAndRedactsEvidence(t *testing.T) {
	a, err := parseAnalysis("```json\n" + `{"impact_summary":"workload unavailable","recommendation":"password=private inspect configuration","proposed_action":"run_shell","confidence":"certain","impact_scope":"everywhere"}` + "\n```")
	if err != nil || a.ProposedAction != "none" || a.Confidence != "low" || a.ImpactScope != "unknown" ||
		strings.Contains(a.Recommendation, "private") {
		t.Fatalf("unsafe model response accepted: %+v / %v", a, err)
	}
	if _, err := parseAnalysis(`{"proposed_action":"restart_workload"}`); err == nil {
		t.Fatal("incomplete model response accepted")
	}
	evidence := alertEvidence(&Alert{Alertname: "failure", Labels: map[string]string{"namespace": "prod", "password": "private"},
		Annotations: map[string]string{"description": "Bearer confidential-token", "untrusted": "private"}})
	raw, _ := json.Marshal(evidence)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "confidential-token") {
		t.Fatal("diagnostic evidence leaked a secret or a non-allowlisted field")
	}
	for _, text := range []string{"Authorization: Bearer confidential-token", "authorization=Basic abc123"} {
		if redacted := safeEvidence(text, 128); strings.Contains(redacted, "confidential-token") || strings.Contains(redacted, "abc123") {
			t.Fatalf("authorization credentials were not fully redacted: %q", redacted)
		}
	}
	jsonSecrets := `{"password":"json-secret value","api_key":"json-key","authorization":"Basic basic-secret"}`
	redactedJSON := safeEvidence(jsonSecrets, 256)
	for _, secret := range []string{"json-secret", "json-key", "basic-secret"} {
		if strings.Contains(redactedJSON, secret) {
			t.Fatalf("JSON credentials were not redacted: %q", redactedJSON)
		}
	}
	if len([]rune(safeEvidence(strings.Repeat("a", 100), 8))) != 11 {
		t.Fatal("evidence truncation is not bounded")
	}
}

func TestContainerDiagnosisEvidenceIncludesCurrentAndLastTermination(t *testing.T) {
	pod := corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", Ready: true, RestartCount: 1}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "api", RestartCount: 4,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "token=hidden"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 137, Message: "password=secret",
			}},
		}},
	}}
	evidence := containerDiagnosisEvidence(pod)
	raw, _ := json.Marshal(evidence)
	if len(evidence) != 2 || !strings.Contains(string(raw), "CrashLoopBackOff") ||
		!strings.Contains(string(raw), `"exit_code":137`) ||
		strings.Contains(string(raw), "hidden") || strings.Contains(string(raw), "secret") {
		t.Fatalf("container evidence missing useful state or leaked a secret: %s", raw)
	}
}

func TestDeploymentDiagnosisEvidenceBoundsAndRedactsConditions(t *testing.T) {
	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 7},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Paused: true, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 6, Replicas: 3, UpdatedReplicas: 2, ReadyReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 2,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "ProgressDeadlineExceeded", Message: "authorization: Bearer hidden-token"}},
		},
	}
	evidence := deploymentDiagnosisEvidence(dep)
	raw, _ := json.Marshal(evidence)
	if evidence["desired_replicas"] != int32(3) || evidence["observed_generation"] != int64(6) ||
		!strings.Contains(string(raw), "ProgressDeadlineExceeded") || strings.Contains(string(raw), "hidden-token") {
		t.Fatalf("deployment evidence incomplete or unsanitized: %s", raw)
	}
}

func TestWarningEventDiagnosisEvidenceMatchesCurrentPodAndIsBounded(t *testing.T) {
	uid := types.UID("pod-current")
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "prod", UID: uid}}
	now := time.Now()
	events := make([]corev1.Event, 0, 25)
	for i := 0; i < 25; i++ {
		events = append(events, corev1.Event{
			Type: corev1.EventTypeWarning, Reason: "BackOff", Message: "token=hidden",
			Count: int32(i + 1), FirstTimestamp: metav1.NewTime(now.Add(-time.Duration(i+1) * time.Minute)),
			LastTimestamp:  metav1.NewTime(now.Add(time.Duration(i) * time.Second)),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: uid},
		})
	}
	events = append(events,
		corev1.Event{Type: corev1.EventTypeNormal, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: uid}},
		corev1.Event{Type: corev1.EventTypeWarning, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: "pod-old"}},
	)
	evidence := warningEventDiagnosisEvidence(events, pod)
	raw, _ := json.Marshal(evidence)
	if len(evidence) != maxDiagnosisWarningEvents || strings.Contains(string(raw), "hidden") ||
		evidence[0]["count"] != int32(25) {
		t.Fatalf("warning event evidence was not filtered, sorted, bounded, or sanitized: %s", raw)
	}
}

func TestNotificationDecisionLinksCannotExecuteChanges(t *testing.T) {
	raw, _ := json.Marshal(analysis{ImpactSummary: "one application", Recommendation: "restart", ProposedAction: "restart_workload"})
	d := &Diagnosis{AlertID: 42, Status: "ready", Analysis: raw}
	text := diagnosisNotification(d, aiopsPolicy{PublicURL: "https://matrix.example"})
	if !strings.Contains(text, "/#/observe/alerts/42?decision=manual") ||
		!strings.Contains(text, "/#/observe/alerts/42?decision=ai") ||
		strings.Contains(text, "/api/") || strings.Contains(text, "token=") {
		t.Fatal("notification must link to authenticated UI confirmation, never a mutation endpoint")
	}
	d.Status = "succeeded"
	if strings.Contains(diagnosisNotification(d, aiopsPolicy{PublicURL: "https://matrix.example"}), "decision=ai") {
		t.Fatal("completed remediation must not invite another AI execution")
	}
}

func TestDiagnosisHistoryScopeRequiresRequestedAndAuthorizedEnvironment(t *testing.T) {
	where, args := diagnosisHistoryScope(nil, "")
	if where != "FALSE" || len(args) != 0 {
		t.Fatalf("missing principal must not expose diagnosis history: %q %#v", where, args)
	}

	where, args = diagnosisHistoryScope(&rbac.Principal{EnvScope: []string{"prod", "staging"}}, "prod")
	if !strings.Contains(where, "ds.env = $1") || !strings.Contains(where, "ds.env = ANY($2)") ||
		len(args) != 2 || args[0] != "prod" {
		t.Fatalf("requested environment was not intersected with RBAC scope: %q %#v", where, args)
	}

	where, args = diagnosisHistoryScope(&rbac.Principal{EnvScope: []string{}}, "")
	if !strings.Contains(where, "ds.env = ANY($1)") || len(args) != 1 || len(args[0].([]string)) != 0 {
		t.Fatalf("empty environment scope must match no rows: %q %#v", where, args)
	}

	where, args = diagnosisHistoryScope(&rbac.Principal{EnvScope: []string{"*"}}, "")
	if strings.Contains(where, "ds.env") || len(args) != 0 {
		t.Fatalf("wildcard environment scope should not add a restrictive predicate: %q %#v", where, args)
	}
}

func TestDiagnosisHistoryPageSizeAndProjectionAreBounded(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 20}, {20, 20}, {50, 50}, {200, 50}} {
		if got := diagnosisHistorySize(tc.in); got != tc.want {
			t.Errorf("diagnosisHistorySize(%d)=%d want %d", tc.in, got, tc.want)
		}
	}

	raw := json.RawMessage(`{"impact_summary":"Bearer hidden-token","recommendation":"password=hidden inspect rollout","risk":"token=private","evidence":"raw sensitive payload","target":{"uid":"private"}}`)
	summary, recommendation, risk := diagnosisHistoryAnalysis(raw)
	item := diagnosisHistoryItem{
		ID: 1, AlertID: 42, AlertName: "CrashLoopBackOff", Environment: "prod",
		ImpactSummary: summary, Recommendation: recommendation, Risk: risk,
	}
	out, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, forbidden := range []string{"hidden-token", "hidden ", "private", "raw sensitive payload", `"evidence"`, `"target"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("history response leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "inspect rollout") {
		t.Fatalf("allowlisted recommendation missing from history projection: %s", text)
	}
}

func TestHealthFingerprintSeparatesClusterAndResourceIdentity(t *testing.T) {
	f := healthFinding{Kind: "Pod", Namespace: "prod", Name: "web", UID: "uid-1", Reason: "CrashLoopBackOff"}
	a := labelFingerprint(findingLabels(healthCluster{ID: 1, Name: "a"}, f))
	b := labelFingerprint(findingLabels(healthCluster{ID: 2, Name: "b"}, f))
	f.UID = "uid-2"
	c := labelFingerprint(findingLabels(healthCluster{ID: 1, Name: "a"}, f))
	if a == b || a == c {
		t.Fatal("alerts must never merge different clusters or recreated resources")
	}
}
