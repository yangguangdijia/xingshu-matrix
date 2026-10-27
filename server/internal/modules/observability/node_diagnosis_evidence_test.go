package observability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNodeDiagnosisEvidenceReportsConditionsAndOnlyBoundNodeWorkloads(t *testing.T) {
	controller := true
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: "node-uid"},
		Spec:       corev1.NodeSpec{Unschedulable: true, Taints: []corev1.Taint{{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule}}},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse,
				Reason: "KubeletNotReady", Message: "authorization=secret-token"}},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
			NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: "v1.31.2", ContainerRuntimeVersion: "containerd://1.7.0"},
		},
	}
	apiPod := func(name, phase string, ready bool, nodeName string) *corev1.Pod {
		condition := corev1.ConditionFalse
		if ready {
			condition = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "api-7d9f", Controller: &controller},
			}},
			Spec: corev1.PodSpec{NodeName: nodeName},
			Status: corev1.PodStatus{Phase: corev1.PodPhase(phase), Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: condition},
			}},
		}
	}
	cs := fake.NewSimpleClientset(node,
		apiPod("api-1", "Running", true, "worker-1"),
		apiPod("api-2", "Pending", false, "worker-1"),
		apiPod("api-other-node", "Running", true, "worker-2"))

	evidence := nodeDiagnosisEvidence(context.Background(), cs, "worker-1")
	raw, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-token") || !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("node evidence leaked an alert credential: %s", raw)
	}
	status, ok := evidence["node_status"].(map[string]any)
	if !ok || status["unschedulable"] != true || status["uid"] != "node-uid" {
		t.Fatalf("node live status missing: %#v", evidence["node_status"])
	}
	impact, ok := evidence["node_workload_impact"].(map[string]any)
	if !ok || impact["pod_count"] != 2 || impact["workload_count"] != 1 {
		t.Fatalf("node workload impact was not scoped to the selected node: %#v", evidence["node_workload_impact"])
	}
	phases, _ := impact["pod_phases"].(map[string]int)
	if phases["Running"] != 1 || phases["Pending"] != 1 {
		t.Fatalf("node Pod phases missing: %#v", impact["pod_phases"])
	}
	workloads, _ := impact["workloads"].([]map[string]any)
	if len(workloads) != 1 || workloads[0]["name"] != "api-7d9f" ||
		workloads[0]["pods"] != 2 || workloads[0]["ready_pods"] != 1 {
		t.Fatalf("affected workload summary incorrect: %#v", impact["workloads"])
	}
}
