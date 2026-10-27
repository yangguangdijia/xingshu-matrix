package observability

import (
	"context"
	"io"
	"math"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

const maxDiagnosisWarningEvents = 20
const maxDiagnosisLogContainers = 4
const maxDiagnosisLogBytes = int64(4 << 10)
const maxDiagnosisLogTailLines = int64(80)
const diagnosisLogReadTimeout = 5 * time.Second
const maxDiagnosisNodeWorkloads = 30
const maxDiagnosisNodePods = int64(2000)

func containerDiagnosisEvidence(pod corev1.Pod) []map[string]any {
	out := make([]map[string]any, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	appendStatus := func(status corev1.ContainerStatus) {
		if len(out) >= 30 {
			return
		}
		state := map[string]any{"type": "unknown"}
		switch {
		case status.State.Waiting != nil:
			state = map[string]any{"type": "waiting", "reason": safeEvidence(status.State.Waiting.Reason, 160),
				"message": safeEvidence(status.State.Waiting.Message, 600)}
		case status.State.Terminated != nil:
			state = terminatedDiagnosisEvidence(status.State.Terminated)
		case status.State.Running != nil:
			state = map[string]any{"type": "running", "started_at": status.State.Running.StartedAt.UTC().Format(time.RFC3339)}
		}
		item := map[string]any{"name": safeEvidence(status.Name, 128), "ready": status.Ready,
			"restart_count": status.RestartCount, "state": state}
		if last := status.LastTerminationState.Terminated; last != nil {
			item["last_termination"] = terminatedDiagnosisEvidence(last)
		}
		out = append(out, item)
	}
	for _, status := range pod.Status.InitContainerStatuses {
		appendStatus(status)
	}
	for _, status := range pod.Status.ContainerStatuses {
		appendStatus(status)
	}
	return out
}

func containerDiagnosisLogEvidence(ctx context.Context, cs kubernetes.Interface, pod corev1.Pod) []map[string]any {
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	sort.SliceStable(statuses, func(i, j int) bool {
		return diagnosisContainerNeedsLogs(statuses[i]) && !diagnosisContainerNeedsLogs(statuses[j])
	})
	out := make([]map[string]any, 0, min(len(statuses), maxDiagnosisLogContainers))
	attempted := 0
	for _, status := range statuses {
		if attempted >= maxDiagnosisLogContainers {
			break
		}
		if status.Name == "" {
			continue
		}
		attempted++
		previous := status.RestartCount > 0
		body, err := readDiagnosisContainerLogs(ctx, cs, pod, status.Name, previous)
		if err != nil && previous {
			previous = false
			body, err = readDiagnosisContainerLogs(ctx, cs, pod, status.Name, false)
		}
		if err != nil {
			out = append(out, map[string]any{"container": safeEvidence(status.Name, 128), "available": false})
			continue
		}
		truncated := len(body) > int(maxDiagnosisLogBytes)
		if truncated {
			body = body[:maxDiagnosisLogBytes]
		}
		if len(body) == 0 {
			continue
		}
		out = append(out, map[string]any{
			"container": safeEvidence(status.Name, 128), "previous": previous,
			"truncated": truncated, "content": safeEvidence(string(body), 1400),
		})
	}
	return out
}

func diagnosisContainerNeedsLogs(status corev1.ContainerStatus) bool {
	return status.RestartCount > 0 || status.State.Waiting != nil || status.State.Terminated != nil
}

func readDiagnosisContainerLogs(ctx context.Context, cs kubernetes.Interface, pod corev1.Pod, container string, previous bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, diagnosisLogReadTimeout)
	defer cancel()
	tail, limit := maxDiagnosisLogTailLines, maxDiagnosisLogBytes
	stream, err := cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container, Previous: previous, Timestamps: true, TailLines: &tail, LimitBytes: &limit,
	}).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return io.ReadAll(io.LimitReader(stream, maxDiagnosisLogBytes+1))
}

func nodeDiagnosisEvidence(ctx context.Context, cs kubernetes.Interface, name string) map[string]any {
	evidence := map[string]any{"node_name": safeEvidence(name, 128)}
	node, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		evidence["node_status"] = "unavailable"
		evidence["node_workload_impact"] = "unavailable"
		return evidence
	}
	conditions := make([]map[string]any, 0, len(node.Status.Conditions))
	for i, condition := range node.Status.Conditions {
		if i >= 20 {
			break
		}
		conditions = append(conditions, map[string]any{
			"type": safeEvidence(string(condition.Type), 64), "status": safeEvidence(string(condition.Status), 16),
			"reason": safeEvidence(condition.Reason, 128), "message": safeEvidence(condition.Message, 500),
			"last_transition": condition.LastTransitionTime.UTC().Format(time.RFC3339),
		})
	}
	addresses := make([]map[string]string, 0, len(node.Status.Addresses))
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP || address.Type == corev1.NodeExternalIP {
			addresses = append(addresses, map[string]string{"type": string(address.Type), "address": safeEvidence(address.Address, 128)})
		}
	}
	capacity := map[string]string{}
	for _, resource := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage, corev1.ResourcePods} {
		if quantity, ok := node.Status.Allocatable[resource]; ok {
			capacity[string(resource)] = quantity.String()
		}
	}
	taints := make([]map[string]string, 0, len(node.Spec.Taints))
	for _, taint := range node.Spec.Taints {
		taints = append(taints, map[string]string{"key": safeEvidence(taint.Key, 128), "value": safeEvidence(taint.Value, 256), "effect": string(taint.Effect)})
	}
	evidence["node_status"] = map[string]any{
		"uid": string(node.UID), "unschedulable": node.Spec.Unschedulable, "conditions": conditions,
		"addresses": addresses, "allocatable": capacity, "taints": taints,
		"kubelet_version": safeEvidence(node.Status.NodeInfo.KubeletVersion, 64),
		"runtime_version": safeEvidence(node.Status.NodeInfo.ContainerRuntimeVersion, 96),
		"kernel_version":  safeEvidence(node.Status.NodeInfo.KernelVersion, 96),
		"os_image":        safeEvidence(node.Status.NodeInfo.OSImage, 128),
	}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", name).String(),
		Limit:         maxDiagnosisNodePods,
	})
	if err != nil {
		evidence["node_workload_impact"] = "unavailable"
		return evidence
	}
	evidence["node_workload_impact"] = summarizeNodeWorkloadImpact(name, pods.Items, pods.Continue != "")
	return evidence
}

func hostAgentDiagnosisEvidence(ctx context.Context, env, nodeName string, cs kubernetes.Interface, provider app.HostDiagnosticProvider) map[string]any {
	unavailable := map[string]any{"status": "unavailable"}
	if provider == nil || nodeName == "" {
		return unavailable
	}
	node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return unavailable
	}
	ips := make([]string, 0, 1)
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP && address.Address != "" {
			ips = append(ips, address.Address)
		}
	}
	if len(ips) == 0 {
		return unavailable
	}
	lookup, err := provider.Snapshot(ctx, env, ips)
	if err != nil {
		return unavailable
	}
	switch lookup.Status {
	case "not_registered", "ambiguous", "offline", "stale":
		return map[string]any{"status": lookup.Status}
	case "available":
	default:
		return unavailable
	}
	s := lookup.Snapshot
	matched := false
	for _, ip := range ips {
		if ip == s.IP {
			matched = true
			break
		}
	}
	age := time.Since(s.LastSeenAt)
	if !matched || s.LastSeenAt.IsZero() || age < -30*time.Second || age >= 90*time.Second {
		return map[string]any{"status": "stale"}
	}

	disks := make([]map[string]any, 0, min(len(s.Disks), 8))
	for _, disk := range s.Disks {
		if len(disks) == 8 {
			break
		}
		disks = append(disks, map[string]any{
			"mount": safeEvidence(disk.Mount, 128), "filesystem": safeEvidence(disk.FSType, 64),
			"total_bytes": disk.Total, "used_bytes": disk.Used, "used_percent": finiteEvidenceNumber(disk.Percent),
		})
	}
	return map[string]any{
		"status": "available", "host": safeEvidence(s.Name, 128), "ip": safeEvidence(s.IP, 64),
		"agent_version": safeEvidence(s.AgentVersion, 64), "sampled_at": s.LastSeenAt.UTC().Format(time.RFC3339),
		"os": safeEvidence(s.OS, 64), "platform": safeEvidence(s.Platform, 64),
		"platform_version": safeEvidence(s.PlatformVer, 64), "kernel": safeEvidence(s.Kernel, 96),
		"architecture": safeEvidence(s.Arch, 32), "cpu_cores": s.CPUCores,
		"cpu_percent": finiteEvidenceNumber(s.CPUPercent), "load_1m": finiteEvidenceNumber(s.Load1),
		"memory_total_bytes": s.MemTotal, "memory_used_bytes": s.MemUsed,
		"memory_used_percent": finiteEvidenceNumber(s.MemPercent), "uptime_seconds": s.Uptime, "disks": disks,
	}
}

func hostAgentInspectionEvidence(ctx context.Context, env, nodeName string, cs kubernetes.Interface, provider app.HostDiagnosticInspectionProvider) map[string]any {
	if provider == nil || nodeName == "" {
		return map[string]any{"status": "unavailable"}
	}
	node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return map[string]any{"status": "unavailable"}
	}
	ips := make([]string, 0, 1)
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP && address.Address != "" {
			ips = append(ips, address.Address)
		}
	}
	if len(ips) == 0 {
		return map[string]any{"status": "unavailable"}
	}
	inspection, err := provider.Inspect(ctx, env, ips)
	if err != nil {
		return map[string]any{"status": "unavailable"}
	}
	switch inspection.Status {
	case "not_registered", "ambiguous", "offline", "stale", "unsupported":
		return map[string]any{"status": inspection.Status}
	case "available":
	default:
		return map[string]any{"status": "unavailable"}
	}
	age := time.Since(inspection.CapturedAt)
	if inspection.CapturedAt.IsZero() || age < -30*time.Second || age >= 2*time.Minute {
		return map[string]any{"status": "stale"}
	}
	return map[string]any{
		"status": "available", "read_only": true,
		"captured_at": inspection.CapturedAt.UTC().Format(time.RFC3339),
		"output":      safeEvidence(inspection.Output, 8000),
	}
}

func finiteEvidenceNumber(value float64) any {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return value
}

func summarizeNodeWorkloadImpact(nodeName string, pods []corev1.Pod, truncated bool) map[string]any {
	type workload struct {
		namespace string
		kind      string
		name      string
		total     int
		ready     int
	}
	groups := map[string]*workload{}
	phases := map[string]int{}
	podCount := 0
	for _, pod := range pods {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		podCount++
		phase := string(pod.Status.Phase)
		if phase == "" {
			phase = "Unknown"
		}
		phases[phase]++
		kind, name := "Pod", pod.Name
		for _, owner := range pod.OwnerReferences {
			if owner.Controller != nil && *owner.Controller {
				kind, name = owner.Kind, owner.Name
				break
			}
		}
		key := pod.Namespace + "\x00" + kind + "\x00" + name
		item := groups[key]
		if item == nil {
			item = &workload{namespace: pod.Namespace, kind: kind, name: name}
			groups[key] = item
		}
		item.total++
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				item.ready++
				break
			}
		}
	}
	items := make([]workload, 0, len(groups))
	for _, item := range groups {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].namespace != items[j].namespace {
			return items[i].namespace < items[j].namespace
		}
		if items[i].kind != items[j].kind {
			return items[i].kind < items[j].kind
		}
		return items[i].name < items[j].name
	})
	out := make([]map[string]any, 0, min(len(items), maxDiagnosisNodeWorkloads))
	for i, item := range items {
		if i >= maxDiagnosisNodeWorkloads {
			truncated = true
			break
		}
		out = append(out, map[string]any{
			"namespace": safeEvidence(item.namespace, 128), "kind": safeEvidence(item.kind, 64),
			"name": safeEvidence(item.name, 128), "pods": item.total, "ready_pods": item.ready,
		})
	}
	phaseCounts := map[string]int{}
	for phase, count := range phases {
		phaseCounts[safeEvidence(phase, 64)] = count
	}
	return map[string]any{"node_name": safeEvidence(nodeName, 128), "pod_count": podCount,
		"pod_phases": phaseCounts, "workloads": out, "truncated": truncated,
		"workload_count": len(groups)}
}

func terminatedDiagnosisEvidence(state *corev1.ContainerStateTerminated) map[string]any {
	if state == nil {
		return map[string]any{"type": "unknown"}
	}
	return map[string]any{"type": "terminated", "reason": safeEvidence(state.Reason, 160),
		"message": safeEvidence(state.Message, 600), "exit_code": state.ExitCode,
		"started_at":  state.StartedAt.UTC().Format(time.RFC3339),
		"finished_at": state.FinishedAt.UTC().Format(time.RFC3339)}
}

func deploymentDiagnosisEvidence(dep *appsv1.Deployment) map[string]any {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	strategy := dep.Spec.Strategy.Type
	if strategy == "" {
		strategy = appsv1.RollingUpdateDeploymentStrategyType
	}
	conditions := make([]map[string]any, 0, len(dep.Status.Conditions))
	for i, condition := range dep.Status.Conditions {
		if i >= 10 {
			break
		}
		conditions = append(conditions, map[string]any{
			"type": safeEvidence(string(condition.Type), 96), "status": safeEvidence(string(condition.Status), 32),
			"reason": safeEvidence(condition.Reason, 160), "message": safeEvidence(condition.Message, 600),
			"last_transition": condition.LastTransitionTime.UTC().Format(time.RFC3339),
		})
	}
	return map[string]any{
		"generation": dep.Generation, "observed_generation": dep.Status.ObservedGeneration,
		"desired_replicas": desired, "replicas": dep.Status.Replicas,
		"updated_replicas": dep.Status.UpdatedReplicas, "ready_replicas": dep.Status.ReadyReplicas,
		"available_replicas": dep.Status.AvailableReplicas, "unavailable_replicas": dep.Status.UnavailableReplicas,
		"paused": dep.Spec.Paused, "strategy": safeEvidence(string(strategy), 64),
		"conditions": conditions,
	}
}

func warningEventDiagnosisEvidence(events []corev1.Event, pod corev1.Pod) []map[string]any {
	matched := make([]corev1.Event, 0, len(events))
	for _, event := range events {
		ref := event.InvolvedObject
		if event.Type != corev1.EventTypeWarning || ref.Kind != "Pod" ||
			ref.Namespace != pod.Namespace || ref.Name != pod.Name || ref.UID != pod.UID {
			continue
		}
		matched = append(matched, event)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return diagnosisEventTime(matched[i]).After(diagnosisEventTime(matched[j]))
	})
	if len(matched) > maxDiagnosisWarningEvents {
		matched = matched[:maxDiagnosisWarningEvents]
	}
	out := make([]map[string]any, 0, len(matched))
	for _, event := range matched {
		out = append(out, map[string]any{
			"reason": safeEvidence(event.Reason, 160), "message": safeEvidence(event.Message, 800),
			"count": event.Count, "first_seen": event.FirstTimestamp.UTC().Format(time.RFC3339),
			"last_seen": diagnosisEventTime(event).UTC().Format(time.RFC3339),
		})
	}
	return out
}

func diagnosisEventTime(event corev1.Event) time.Time {
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	return event.CreationTimestamp.Time
}
