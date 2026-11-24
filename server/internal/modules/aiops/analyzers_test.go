package aiops

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var allowedAnalyzerActions = map[string]bool{
	"k8s.restart": true, "k8s.scale": true, "k8s.set_resources": true, "k8s.set_image": true,
	"k8s.rollback_deployment": true, "k8s.cordon": true, "k8s.drain": true, "k8s.delete_pod": true,
	"k8s.describe": true, "k8s.logs": true, "k8s.events": true,
}

var readOnlyAnalyzerActions = map[string]bool{"k8s.describe": true, "k8s.logs": true, "k8s.events": true}

func tPod(name string, mut func(p *corev1.Pod)) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Labels: map[string]string{"pod-template-hash": "abc"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if mut != nil {
		mut(&p)
	}
	return p
}

func tDeploy(rev string, mut func(d *appsv1.Deployment)) appsv1.Deployment {
	r := int32(2)
	d := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web",
			Annotations: map[string]string{"deployment.kubernetes.io/revision": rev}},
		Spec:   appsv1.DeploymentSpec{Replicas: &r},
		Status: appsv1.DeploymentStatus{Replicas: 2, AvailableReplicas: 2, UpdatedReplicas: 2},
	}
	if mut != nil {
		mut(&d)
	}
	return d
}

func tRS() appsv1.ReplicaSet {
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-abc",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
}

func tEvent(kind, name, reason, msg string, at int) corev1.Event {
	return corev1.Event{
		InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: "default", Name: name},
		Reason:         reason, Message: msg, Type: "Warning", Count: 1,
		LastTimestamp: metav1.NewTime(time.Date(2026, 1, 1, 0, at, 0, 0, time.UTC)),
	}
}

func waiting(reason, msg string, restarts int32) func(p *corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Image: "nginx:1", RestartCount: restarts,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}}
	}
}

func TestAnalyzers(t *testing.T) {
	memLimit := resource.MustParse("512Mi")
	cases := []struct {
		name         string
		in           AnalyzerInput
		analyzer     string
		wantConf     string
		wantMsg      string // substring of Message
		wantEvidence string // substring of some evidence line
		wantAction   string // some step action in the plan ("" = read-only plan)
		wantVerify   bool   // last verify step is k8s.wait_ready
	}{
		{
			name: "crashloop with rollback",
			in: AnalyzerInput{
				Pods: []corev1.Pod{tPod("web-abc-1", func(p *corev1.Pod) {
					waiting("CrashLoopBackOff", "back-off", 4)(p)
					p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}
				})},
				ReplicaSets: []appsv1.ReplicaSet{tRS()},
				Deployments: []appsv1.Deployment{tDeploy("3", nil)},
			},
			analyzer: analyzerCrashLoop, wantConf: confHigh, wantMsg: "上次退出码 1", wantEvidence: "已重启 4 次",
			wantAction: "k8s.rollback_deployment", wantVerify: true,
		},
		{
			name: "crashloop standalone pod read-only",
			in: AnalyzerInput{Pods: []corev1.Pod{tPod("solo", func(p *corev1.Pod) {
				p.OwnerReferences = nil
				waiting("CrashLoopBackOff", "", 2)(p)
			})}},
			analyzer: analyzerCrashLoop, wantConf: confHigh, wantMsg: "退出原因未知",
		},
		{
			name: "oomkilled raises memory",
			in: AnalyzerInput{
				Pods: []corev1.Pod{tPod("web-abc-2", func(p *corev1.Pod) {
					p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: memLimit}
					p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3,
						State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
						LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}
				})},
				Deployments: []appsv1.Deployment{tDeploy("1", nil)}, // resolved via pod-template-hash
			},
			analyzer: analyzerOOMKilled, wantConf: confHigh, wantMsg: "OOMKilled", wantEvidence: "768Mi",
			wantAction: "k8s.set_resources", wantVerify: true,
		},
		{
			name: "image pull backoff with rollback",
			in: AnalyzerInput{
				Pods:        []corev1.Pod{tPod("web-abc-3", waiting("ImagePullBackOff", "Back-off pulling image", 0))},
				Events:      []corev1.Event{tEvent("Pod", "web-abc-3", "Failed", "Failed to pull image \"nginx:1\": not found", 1)},
				ReplicaSets: []appsv1.ReplicaSet{tRS()},
				Deployments: []appsv1.Deployment{tDeploy("2", nil)},
			},
			analyzer: analyzerImagePull, wantConf: confHigh, wantMsg: "ImagePullBackOff", wantEvidence: "nginx:1",
			wantAction: "k8s.rollback_deployment", wantVerify: true,
		},
		{
			name: "err image pull first revision read-only",
			in: AnalyzerInput{
				Pods:        []corev1.Pod{tPod("web-abc-3", waiting("ErrImagePull", "", 0))},
				Deployments: []appsv1.Deployment{tDeploy("1", nil)},
			},
			analyzer: analyzerImagePull, wantConf: confHigh, wantMsg: "ErrImagePull",
		},
		{
			name: "pending insufficient resources",
			in: AnalyzerInput{
				Pods: []corev1.Pod{tPod("big", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })},
				Events: []corev1.Event{tEvent("Pod", "big", "FailedScheduling",
					"0/3 nodes are available: 3 Insufficient memory.", 1)},
			},
			analyzer: analyzerPending, wantConf: confHigh, wantMsg: "资源不足", wantEvidence: "Insufficient memory",
		},
		{
			name: "pending taint via condition",
			in: AnalyzerInput{Pods: []corev1.Pod{tPod("t", func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: "Unschedulable", Message: "0/1 nodes are available: 1 node(s) had untolerated taint {dedicated: gpu}."}}
			})}},
			analyzer: analyzerPending, wantConf: confHigh, wantMsg: "污点",
		},
		{
			name: "pending affinity",
			in: AnalyzerInput{
				Pods: []corev1.Pod{tPod("a", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })},
				Events: []corev1.Event{tEvent("Pod", "a", "FailedScheduling",
					"0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.", 1)},
			},
			analyzer: analyzerPending, wantConf: confHigh, wantMsg: "亲和性",
		},
		{
			name: "pending unbound pvc",
			in: AnalyzerInput{
				Pods: []corev1.Pod{tPod("db", func(p *corev1.Pod) {
					p.Status.Phase = corev1.PodPending
					p.Spec.Volumes = []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
				})},
				Events: []corev1.Event{tEvent("Pod", "db", "FailedScheduling",
					"0/2 nodes are available: pod has unbound immediate PersistentVolumeClaims.", 1)},
			},
			analyzer: analyzerPending, wantConf: confHigh, wantMsg: "PVC 未绑定",
		},
		{
			name: "liveness probe failed",
			in: AnalyzerInput{
				Pods: []corev1.Pod{tPod("p1", nil)},
				Events: []corev1.Event{
					tEvent("Pod", "p1", "Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500", 1),
					tEvent("Pod", "p1", "Unhealthy", "Readiness probe failed: connection refused", 2),
				},
			},
			analyzer: analyzerProbeFailed, wantConf: confMedium, wantMsg: "存活探针", wantEvidence: "失败 2 次",
		},
		{
			name: "configmap missing",
			in: AnalyzerInput{
				Pods: []corev1.Pod{tPod("c1", func(p *corev1.Pod) {
					p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}}}
					p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "PW", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"}, Key: "pw"}}}}
					waiting("CreateContainerConfigError", "configmap \"app-config\" not found", 0)(p)
				})},
				ConfigMaps: []string{"other"},
				Secrets:    []string{"db-secret"},
			},
			analyzer: analyzerConfigMissing, wantConf: confHigh, wantMsg: "ConfigMap app-config",
		},
		{
			name: "node not ready",
			in: AnalyzerInput{Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, Reason: "NodeStatusUnknown"}}}}}},
			analyzer: analyzerNodePressure, wantConf: confHigh, wantMsg: "NotReady", wantEvidence: "NodeStatusUnknown",
			wantAction: "k8s.drain", wantVerify: true,
		},
		{
			name: "node disk pressure",
			in: AnalyzerInput{Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n2"},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
					{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue}}}}}},
			analyzer: analyzerNodePressure, wantConf: confHigh, wantMsg: "DiskPressure",
			wantAction: "k8s.cordon", wantVerify: true,
		},
		{
			name: "deployment progress deadline exceeded",
			in: AnalyzerInput{Deployments: []appsv1.Deployment{tDeploy("5", func(d *appsv1.Deployment) {
				d.Status.AvailableReplicas, d.Status.UnavailableReplicas = 1, 1
				d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing,
					Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "ReplicaSet web-xyz has timed out progressing."}}
			})}},
			analyzer: analyzerDeployment, wantConf: confHigh, wantMsg: "ProgressDeadlineExceeded", wantEvidence: "不可用 1",
			wantAction: "k8s.rollback_deployment", wantVerify: true,
		},
		{
			name: "deployment unavailable read-only",
			in: AnalyzerInput{Deployments: []appsv1.Deployment{tDeploy("1", func(d *appsv1.Deployment) {
				d.Status.AvailableReplicas, d.Status.UnavailableReplicas = 0, 2
			})}},
			analyzer: analyzerDeployment, wantConf: confMedium, wantMsg: "副本不可用",
		},
		{
			name: "frequent restarts",
			in: AnalyzerInput{Pods: []corev1.Pod{tPod("r1", func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 7,
					State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}}}}
			})}},
			analyzer: analyzerRestarts, wantConf: confMedium, wantMsg: "7 次", wantEvidence: "上次退出码 2",
		},
		{
			name: "pvc pending",
			in: AnalyzerInput{
				PVCs: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "data"},
					Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}},
				Events: []corev1.Event{tEvent("PersistentVolumeClaim", "data", "ProvisioningFailed", "storageclass \"fast\" not found", 1)},
			},
			analyzer: analyzerPVCPending, wantConf: confHigh, wantMsg: "PVC 未绑定", wantEvidence: "ProvisioningFailed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := runAnalyzers(tc.in)
			var f *Finding
			for i := range fs {
				if fs[i].Analyzer == tc.analyzer {
					f = &fs[i]
					break
				}
			}
			if f == nil {
				t.Fatalf("no %s finding; got %+v", tc.analyzer, fs)
			}
			if got := findingConfidence(*f); got != tc.wantConf {
				t.Errorf("confidence = %s, want %s", got, tc.wantConf)
			}
			if !strings.Contains(f.Message, tc.wantMsg) {
				t.Errorf("message %q lacks %q", f.Message, tc.wantMsg)
			}
			if tc.wantEvidence != "" && !strings.Contains(strings.Join(f.Evidence, "\n"), tc.wantEvidence) {
				t.Errorf("evidence %q lacks %q", f.Evidence, tc.wantEvidence)
			}
			if f.Object == "" || f.Severity == "" || len(f.Plans) != 1 {
				t.Fatalf("incomplete finding %+v", f)
			}
			plan := f.Plans[0]
			if plan.Title == "" || len(plan.Steps) == 0 {
				t.Fatalf("empty plan %+v", plan)
			}
			readOnly, found := true, false
			for _, s := range plan.Steps {
				if !allowedAnalyzerActions[s.Action] {
					t.Errorf("step uses unregistered action %q", s.Action)
				}
				if !readOnlyAnalyzerActions[s.Action] {
					readOnly = false
				}
				if s.Action == tc.wantAction {
					found = true
				}
			}
			if tc.wantAction == "" && !readOnly {
				t.Errorf("expected read-only plan, got %+v", plan.Steps)
			}
			if tc.wantAction != "" && !found {
				t.Errorf("plan lacks %s: %+v", tc.wantAction, plan.Steps)
			}
			hasVerify := len(plan.Verify) > 0 && plan.Verify[len(plan.Verify)-1].Action == "k8s.wait_ready"
			if hasVerify != tc.wantVerify {
				t.Errorf("verify wait_ready = %v, want %v (%+v)", hasVerify, tc.wantVerify, plan.Verify)
			}
			if !readOnly && !hasVerify {
				t.Errorf("change plan must end with k8s.wait_ready")
			}
		})
	}
}

func TestAnalyzersHealthy(t *testing.T) {
	in := AnalyzerInput{
		Pods: []corev1.Pod{tPod("ok", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		})},
		Deployments: []appsv1.Deployment{tDeploy("1", nil)},
		Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}},
	}
	if fs := runAnalyzers(in); len(fs) != 0 {
		t.Fatalf("healthy cluster produced findings: %+v", fs)
	}
}

func TestAnalyzersNoDuplicateCrashloopOOM(t *testing.T) {
	in := AnalyzerInput{Pods: []corev1.Pod{tPod("x", func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 9,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}
	})}}
	fs := runAnalyzers(in)
	if len(fs) != 1 || fs[0].Analyzer != analyzerOOMKilled {
		t.Fatalf("want only oomkilled, got %+v", fs)
	}
}

func TestAggregateFindingsAnalyzers(t *testing.T) {
	mk := func(name, conf, sev string, act string) Finding {
		return analyzerNewFinding(name, sev, conf, "Pod default/"+name, name+" 问题", nil, PlanDraft{
			Title: name, Steps: []StepDraft{analyzerStep(act, analyzerObjParams("pod", "default", name), "")}})
	}
	dup := mk("dup", confMedium, sevWarning, "k8s.describe")
	dup2 := dup
	dup2.Analyzer = "dup2"
	fs := []Finding{
		mk("low", confLow, sevCritical, "k8s.events"),
		dup,
		mk("high-warn", confHigh, sevWarning, "k8s.logs"),
		dup2,
		mk("high-crit", confHigh, sevCritical, "k8s.restart"),
	}
	c, plans := aggregateFindings(fs)
	if c.Confidence != confHigh || !strings.Contains(c.RootCause, "high-crit") {
		t.Errorf("conclusion = %+v", c)
	}
	if len(c.Findings) != 5 || c.Findings[0].Analyzer != "high-crit" || c.Findings[1].Analyzer != "high-warn" || c.Findings[4].Analyzer != "low" {
		t.Errorf("findings not sorted: %v", analyzerNames(c.Findings))
	}
	if len(plans) != 3 {
		t.Fatalf("want 3 plans, got %d", len(plans))
	}
	want := []string{"high-crit", "high-warn", "dup"}
	for i, p := range plans {
		if p.Title != want[i] {
			t.Errorf("plan %d = %s, want %s", i, p.Title, want[i])
		}
	}
	if c.Summary == "" || c.Impact == "" {
		t.Errorf("empty summary/impact: %+v", c)
	}

	// Low-confidence top finding -> root cause uncertain.
	c, _ = aggregateFindings([]Finding{mk("only", confMedium, sevWarning, "k8s.describe")})
	if c.RootCause != "不确定" || len(c.Hypotheses) == 0 || c.Confidence != confMedium {
		t.Errorf("uncertain conclusion = %+v", c)
	}

	// No findings -> read-only plan.
	c, plans = aggregateFindings(nil)
	if c.RootCause != "不确定" || c.Confidence != confLow || len(plans) != 1 {
		t.Fatalf("empty aggregate = %+v %+v", c, plans)
	}
	for _, s := range plans[0].Steps {
		if !readOnlyAnalyzerActions[s.Action] {
			t.Errorf("fallback plan not read-only: %s", s.Action)
		}
	}
	if rulesEngine != "rules" {
		t.Error("engine name")
	}
}

func analyzerNames(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Analyzer
	}
	return out
}
