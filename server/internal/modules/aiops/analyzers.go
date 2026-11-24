package aiops

// Rule analyzers (k8sgpt-style). runAnalyzers is a pure function over an
// AnalyzerInput snapshot: no IO, no clock, deterministic output order.
// aggregateFindings turns findings into a rules-engine Conclusion and at most
// three deduplicated PlanDrafts built only from registered k8s.* actions.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// Analyzer names (Finding.Analyzer).
const (
	analyzerCrashLoop     = "crashloop"
	analyzerOOMKilled     = "oomkilled"
	analyzerImagePull     = "imagepull"
	analyzerPending       = "pending"
	analyzerProbeFailed   = "probe_failed"
	analyzerConfigMissing = "config_missing"
	analyzerNodePressure  = "node_pressure"
	analyzerDeployment    = "deployment_unavailable"
	analyzerRestarts      = "frequent_restarts"
	analyzerPVCPending    = "pvc_pending"
)

// rulesEngine is the engine name for rule-based conclusions. Conclusion has no
// Engine field (see report); callers set Case.Engine = rulesEngine.
const rulesEngine = "rules"

// restartThreshold: containers restarted at least this often are flagged.
const restartThreshold = 5

// Confidence levels. Finding has no Confidence field, so the level is stored
// as the first Evidence line "置信度：<level>" (confEvidencePrefix) and read
// back with findingConfidence.
const (
	confLow    = "low"
	confMedium = "medium"
	confHigh   = "high"

	confEvidencePrefix = "置信度："
	maxEvidence        = 8
	maxPlans           = 3
)

const (
	sevCritical = "critical"
	sevWarning  = "warning"
	sevInfo     = "info"
)

type analyzerFunc func(in AnalyzerInput) []Finding

// analyzers in fixed order; order only matters for ties in aggregation.
var analyzerList = []analyzerFunc{
	analyzeCrashLoop,
	analyzeOOMKilled,
	analyzeImagePull,
	analyzePending,
	analyzeProbeFailed,
	analyzeConfigMissing,
	analyzeNodes,
	analyzeDeployments,
	analyzeRestarts,
	analyzePVCPending,
}

// runAnalyzers runs every rule analyzer over the snapshot.
func runAnalyzers(in AnalyzerInput) []Finding {
	var out []Finding
	for _, a := range analyzerList {
		out = append(out, a(in)...)
	}
	return out
}

// ---------------------------------------------------------------- analyzers

// 1. CrashLoopBackOff (with last exit code / reason).
func analyzeCrashLoop(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.Pods {
		p := &in.Pods[i]
		for _, cs := range analyzerContainerStatuses(p) {
			if cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" {
				continue
			}
			last := cs.LastTerminationState.Terminated
			if last != nil && last.Reason == "OOMKilled" {
				continue // reported by the OOMKilled analyzer
			}
			ev := []string{fmt.Sprintf("容器 %s 处于 CrashLoopBackOff，已重启 %d 次", cs.Name, cs.RestartCount)}
			cause := "退出原因未知"
			if last != nil {
				cause = fmt.Sprintf("上次退出码 %d（%s）", last.ExitCode, analyzerOrDash(last.Reason))
				ev = append(ev, "上次退出："+cause+analyzerTrimMsg(last.Message))
			}
			ev = append(ev, analyzerEventLines(in, "Pod", p.Namespace, p.Name, "BackOff")...)
			kind, ns, name := analyzerWorkloadTarget(in, p)
			var plan PlanDraft
			if kind == "deployment" && analyzerDeploymentRevision(in, ns, name) > 1 {
				plan = PlanDraft{
					Title:       "回滚 Deployment " + ns + "/" + name + " 到上一版本",
					Description: "容器持续崩溃且存在历史版本，先采集上次崩溃日志留证，再回滚到上一个版本并等待就绪。若崩溃与发布无关，请改用只读排查方案。",
					Steps: []StepDraft{
						analyzerStep("k8s.logs", analyzerLogParams(p, cs.Name, true), "保存上次崩溃前的日志"),
						analyzerStep("k8s.rollback_deployment", analyzerObjParams("deployment", ns, name), "回滚到上一版本"),
					},
					Verify: []StepDraft{analyzerWaitReady("deployment", ns, name)},
				}
			} else {
				plan = analyzerReadOnlyPodPlan(p, cs.Name, "排查 "+analyzerPodRef(p)+" 崩溃原因", "崩溃原因需结合日志判断（"+cause+"），先只读采集日志、描述与事件。")
			}
			out = append(out, analyzerNewFinding(analyzerCrashLoop, sevCritical, confHigh, "Pod "+analyzerPodRef(p),
				fmt.Sprintf("容器 %s 反复崩溃（CrashLoopBackOff），%s", cs.Name, cause), ev, plan))
		}
	}
	return out
}

// 2. OOMKilled.
func analyzeOOMKilled(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.Pods {
		p := &in.Pods[i]
		for _, cs := range analyzerContainerStatuses(p) {
			var t *corev1.ContainerStateTerminated
			if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
				t = cs.State.Terminated
			} else if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
				t = cs.LastTerminationState.Terminated
			}
			if t == nil {
				continue
			}
			ev := []string{fmt.Sprintf("容器 %s 因内存超限被杀（OOMKilled，退出码 %d），重启 %d 次", cs.Name, t.ExitCode, cs.RestartCount)}
			limit := analyzerMemoryLimit(p, cs.Name)
			kind, ns, name := analyzerWorkloadTarget(in, p)
			var plan PlanDraft
			conf := confHigh
			if limit > 0 && kind == "deployment" {
				newLimit := analyzerBumpMemory(limit)
				ev = append(ev, fmt.Sprintf("当前内存限制 %s，建议提高到 %s", analyzerFmtMi(limit), newLimit))
				params := analyzerMustJSON(map[string]any{
					"kind": "deployment", "namespace": ns, "name": name, "container": cs.Name,
					"limits": map[string]string{"memory": newLimit},
				})
				plan = PlanDraft{
					Title:       fmt.Sprintf("将 %s/%s 容器 %s 内存限制提高到 %s", ns, name, cs.Name, newLimit),
					Description: "容器多次因内存超限被杀，先保存日志，再把内存限制提高约 50% 并等待滚动完成。若属于内存泄漏，需后续修复应用。",
					Steps: []StepDraft{
						analyzerStep("k8s.logs", analyzerLogParams(p, cs.Name, true), "保存 OOM 前日志"),
						analyzerStep("k8s.set_resources", params, "提高内存限制"),
					},
					Verify: []StepDraft{analyzerWaitReady("deployment", ns, name)},
				}
			} else {
				conf = confMedium
				if limit == 0 {
					ev = append(ev, "容器未设置内存限制，可能是节点内存不足触发的 OOM")
				}
				plan = analyzerReadOnlyPodPlan(p, cs.Name, "排查 "+analyzerPodRef(p)+" 内存超限", "未找到可直接调整的 Deployment 或内存限制，先只读排查内存使用。")
			}
			out = append(out, analyzerNewFinding(analyzerOOMKilled, sevCritical, conf, "Pod "+analyzerPodRef(p),
				fmt.Sprintf("容器 %s 内存超限被杀（OOMKilled）", cs.Name), ev, plan))
		}
	}
	return out
}

// 3. ImagePullBackOff / ErrImagePull.
func analyzeImagePull(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.Pods {
		p := &in.Pods[i]
		for _, cs := range analyzerContainerStatuses(p) {
			w := cs.State.Waiting
			if w == nil || (w.Reason != "ImagePullBackOff" && w.Reason != "ErrImagePull" && w.Reason != "InvalidImageName") {
				continue
			}
			img := cs.Image
			if img == "" {
				img = analyzerSpecImage(p, cs.Name)
			}
			ev := []string{fmt.Sprintf("容器 %s 镜像 %s 拉取失败：%s", cs.Name, img, w.Reason) + analyzerTrimMsg(w.Message)}
			ev = append(ev, analyzerEventLines(in, "Pod", p.Namespace, p.Name, "Failed")...)
			kind, ns, name := analyzerWorkloadTarget(in, p)
			var plan PlanDraft
			if kind == "deployment" && analyzerDeploymentRevision(in, ns, name) > 1 {
				plan = PlanDraft{
					Title:       "回滚 Deployment " + ns + "/" + name + "（镜像无法拉取）",
					Description: "新版本镜像 " + img + " 无法拉取，回滚到上一个可用版本并等待就绪；之后核对镜像地址、标签与拉取凭据。",
					Steps: []StepDraft{
						analyzerStep("k8s.events", analyzerObjParams("pod", p.Namespace, p.Name), "确认拉取失败原因"),
						analyzerStep("k8s.rollback_deployment", analyzerObjParams("deployment", ns, name), "回滚到上一版本"),
					},
					Verify: []StepDraft{analyzerWaitReady("deployment", ns, name)},
				}
			} else {
				plan = PlanDraft{
					Title:       "排查镜像 " + img + " 拉取失败",
					Description: "核对镜像地址/标签是否存在、仓库是否可达、imagePullSecrets 是否正确。确认正确镜像后可用 k8s.set_image 修正。",
					Steps: []StepDraft{
						analyzerStep("k8s.describe", analyzerObjParams("pod", p.Namespace, p.Name), ""),
						analyzerStep("k8s.events", analyzerObjParams("pod", p.Namespace, p.Name), ""),
					},
				}
			}
			out = append(out, analyzerNewFinding(analyzerImagePull, sevCritical, confHigh, "Pod "+analyzerPodRef(p),
				fmt.Sprintf("容器 %s 镜像拉取失败（%s）", cs.Name, w.Reason), ev, plan))
		}
	}
	return out
}

// 4. Pending pods that cannot be scheduled.
func analyzePending(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.Pods {
		p := &in.Pods[i]
		if p.Status.Phase != corev1.PodPending || p.Spec.NodeName != "" {
			continue
		}
		var msgs []string
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
				msgs = append(msgs, c.Message)
			}
		}
		for _, e := range analyzerPodEvents(in, p.Namespace, p.Name) {
			if e.Reason == "FailedScheduling" {
				msgs = append(msgs, e.Message)
			}
		}
		if len(msgs) == 0 {
			continue
		}
		cause, hint := analyzerClassifyScheduling(strings.Join(msgs, "\n"))
		ev := []string{"调度失败：" + analyzerTruncate(msgs[len(msgs)-1], 300)}
		conf := confHigh
		if cause == "未知原因" {
			conf = confMedium
		}
		steps := []StepDraft{
			analyzerStep("k8s.describe", analyzerObjParams("pod", p.Namespace, p.Name), ""),
			analyzerStep("k8s.events", analyzerObjParams("pod", p.Namespace, p.Name), "查看 FailedScheduling 事件"),
		}
		if cause == "资源不足" || cause == "污点不容忍" || cause == "亲和性/节点选择器不匹配" {
			steps = append(steps, analyzerStep("k8s.describe", analyzerMustJSON(map[string]any{"kind": "node"}), "查看各节点可分配资源、污点与标签"))
		}
		for _, cl := range analyzerPvcClaims(p) {
			if cause == "PVC 未绑定" {
				steps = append(steps, analyzerStep("k8s.describe", analyzerObjParams("pvc", p.Namespace, cl), ""))
			}
		}
		plan := PlanDraft{
			Title:       "排查 " + analyzerPodRef(p) + " 调度失败（" + cause + "）",
			Description: hint,
			Steps:       steps,
		}
		out = append(out, analyzerNewFinding(analyzerPending, sevWarning, conf, "Pod "+analyzerPodRef(p),
			"Pod 无法调度："+cause, ev, plan))
	}
	return out
}

func analyzerClassifyScheduling(msg string) (cause, hint string) {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "unbound") && strings.Contains(m, "persistentvolumeclaim"),
		strings.Contains(m, "persistentvolumeclaim") && strings.Contains(m, "not found"):
		return "PVC 未绑定", "Pod 引用的 PVC 未绑定，检查 StorageClass、供应器与 PV。"
	case strings.Contains(m, "insufficient"):
		return "资源不足", "集群可分配资源不足以满足 requests，考虑扩容节点或下调 requests。"
	case strings.Contains(m, "taint"):
		return "污点不容忍", "节点存在 Pod 未容忍的污点，检查污点与 tolerations。"
	case strings.Contains(m, "affinity") || strings.Contains(m, "selector"):
		return "亲和性/节点选择器不匹配", "没有节点满足 nodeSelector/亲和性规则，检查节点标签。"
	}
	return "未知原因", "调度原因不明确，先只读查看事件与节点状态。"
}

// 5. Probe failures (Unhealthy events).
func analyzeProbeFailed(in AnalyzerInput) []Finding {
	type agg struct {
		ns, name string
		count    int32
		liveness bool
		msgs     []string
	}
	byPod := map[string]*agg{}
	var order []string
	for _, e := range analyzerSortedEvents(in.Events) {
		if e.Reason != "Unhealthy" || e.InvolvedObject.Kind != "Pod" {
			continue
		}
		k := e.InvolvedObject.Namespace + "/" + e.InvolvedObject.Name
		a := byPod[k]
		if a == nil {
			a = &agg{ns: e.InvolvedObject.Namespace, name: e.InvolvedObject.Name}
			byPod[k] = a
			order = append(order, k)
		}
		c := e.Count
		if c < 1 {
			c = 1
		}
		a.count += c
		if strings.Contains(e.Message, "Liveness") {
			a.liveness = true
		}
		a.msgs = append(a.msgs, analyzerTruncate(e.Message, 200))
	}
	var out []Finding
	for _, k := range order {
		a := byPod[k]
		kind := "就绪探针"
		sev := sevWarning
		if a.liveness {
			kind = "存活探针"
			sev = sevCritical
		}
		ev := []string{fmt.Sprintf("%s 失败 %d 次", kind, a.count)}
		ev = append(ev, analyzerLastN(a.msgs, 3)...)
		container := ""
		if p := analyzerFindPod(in, a.ns, a.name); p != nil && len(p.Spec.Containers) > 0 {
			container = p.Spec.Containers[0].Name
		}
		plan := PlanDraft{
			Title:       "排查 Pod " + k + " 探针失败",
			Description: "核对探针路径/端口/超时与应用启动耗时，结合日志判断是应用故障还是探针配置过严。",
			Steps: []StepDraft{
				analyzerStep("k8s.describe", analyzerObjParams("pod", a.ns, a.name), "查看探针配置"),
				analyzerStep("k8s.logs", analyzerMustJSON(map[string]any{"namespace": a.ns, "name": a.name, "container": container, "tail_lines": 200}), ""),
				analyzerStep("k8s.events", analyzerObjParams("pod", a.ns, a.name), ""),
			},
		}
		out = append(out, analyzerNewFinding(analyzerProbeFailed, sev, confMedium, "Pod "+k,
			fmt.Sprintf("%s检查持续失败", kind), ev, plan))
	}
	return out
}

// 6. ConfigMap / Secret missing (CreateContainerConfigError).
func analyzeConfigMissing(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.Pods {
		p := &in.Pods[i]
		var waitMsg string
		hit := false
		for _, cs := range analyzerContainerStatuses(p) {
			if w := cs.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
				hit = true
				waitMsg = w.Message
			}
		}
		if !hit {
			for _, e := range analyzerPodEvents(in, p.Namespace, p.Name) {
				if e.Reason == "FailedMount" && strings.Contains(e.Message, "not found") &&
					(strings.Contains(e.Message, "configmap") || strings.Contains(e.Message, "secret")) {
					hit = true
					waitMsg = e.Message
				}
			}
		}
		if !hit {
			continue
		}
		cms, secs := analyzerPodConfigRefs(p)
		var missing []string
		if in.ConfigMaps != nil {
			for _, n := range cms {
				if !analyzerContains(in.ConfigMaps, n) {
					missing = append(missing, "ConfigMap "+n)
				}
			}
		}
		if in.Secrets != nil {
			for _, n := range secs {
				if !analyzerContains(in.Secrets, n) {
					missing = append(missing, "Secret "+n)
				}
			}
		}
		ev := []string{"容器配置错误：" + analyzerOrDash(analyzerTruncate(waitMsg, 200))}
		conf := confMedium
		msg := "容器因配置引用错误无法创建（CreateContainerConfigError）"
		if len(missing) > 0 {
			conf = confHigh
			ev = append(ev, "命名空间中不存在："+strings.Join(missing, "、"))
			msg = "引用的配置不存在：" + strings.Join(missing, "、")
		}
		plan := PlanDraft{
			Title:       "核对 " + analyzerPodRef(p) + " 引用的 ConfigMap/Secret",
			Description: "需人工补齐缺失的 ConfigMap/Secret 或修正引用名（平台不自动创建配置），补齐后 Pod 会自动重试创建。",
			Steps: []StepDraft{
				analyzerStep("k8s.describe", analyzerObjParams("pod", p.Namespace, p.Name), "核对 env/envFrom/volume 引用"),
				analyzerStep("k8s.events", analyzerObjParams("pod", p.Namespace, p.Name), ""),
			},
		}
		out = append(out, analyzerNewFinding(analyzerConfigMissing, sevCritical, conf, "Pod "+analyzerPodRef(p), msg, ev, plan))
	}
	return out
}

// 7. Node NotReady / memory, disk or PID pressure.
func analyzeNodes(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.Nodes {
		n := &in.Nodes[i]
		var notReady *corev1.NodeCondition
		var pressures []string
		for j := range n.Status.Conditions {
			c := &n.Status.Conditions[j]
			switch c.Type {
			case corev1.NodeReady:
				if c.Status != corev1.ConditionTrue {
					notReady = c
				}
			case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
				if c.Status == corev1.ConditionTrue {
					pressures = append(pressures, string(c.Type))
				}
			}
		}
		ref := "Node " + n.Name
		node := analyzerObjParams("node", "", n.Name)
		if notReady != nil {
			ev := []string{fmt.Sprintf("Ready=%s（%s）%s", notReady.Status, analyzerOrDash(notReady.Reason), analyzerTrimMsg(notReady.Message))}
			if len(pressures) > 0 {
				ev = append(ev, "同时存在压力："+strings.Join(pressures, "、"))
			}
			if n.Spec.Unschedulable {
				ev = append(ev, "节点已被标记为不可调度")
			}
			steps := []StepDraft{analyzerStep("k8s.describe", node, "")}
			if !n.Spec.Unschedulable {
				steps = append(steps, analyzerStep("k8s.cordon", node, "停止向该节点调度新 Pod"))
			}
			steps = append(steps, analyzerStep("k8s.drain", node, "驱逐节点上的 Pod 到健康节点"))
			plan := PlanDraft{
				Title:       "隔离并排空故障节点 " + n.Name,
				Description: "节点未就绪，先隔离并驱逐工作负载；随后需人工检查 kubelet/容器运行时/网络，修复后解除隔离。",
				Steps:       steps,
				Verify:      []StepDraft{analyzerWaitReady("node", "", n.Name)},
			}
			out = append(out, analyzerNewFinding(analyzerNodePressure, sevCritical, confHigh, ref, "节点未就绪（NotReady）", ev, plan))
			continue
		}
		if len(pressures) == 0 {
			continue
		}
		ev := []string{"节点压力：" + strings.Join(pressures, "、")}
		steps := []StepDraft{analyzerStep("k8s.describe", node, "查看资源使用与被驱逐的 Pod")}
		if !n.Spec.Unschedulable {
			steps = append(steps, analyzerStep("k8s.cordon", node, "暂停调度，避免压力加剧"))
		}
		steps = append(steps, analyzerStep("k8s.events", node, ""))
		plan := PlanDraft{
			Title:       "隔离压力节点 " + n.Name + " 并排查",
			Description: "节点出现资源压力，先暂停调度新 Pod，再排查占用（日志/镜像/大内存进程），清理后解除隔离。",
			Steps:       steps,
			Verify:      []StepDraft{analyzerWaitReady("node", "", n.Name)},
		}
		out = append(out, analyzerNewFinding(analyzerNodePressure, sevWarning, confHigh, ref,
			"节点存在资源压力："+strings.Join(pressures, "、"), ev, plan))
	}
	return out
}

// 8. Deployment replicas unavailable / rollout stuck.
func analyzeDeployments(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.Deployments {
		d := &in.Deployments[i]
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if want == 0 {
			continue
		}
		stuck := false
		var stuckMsg string
		for _, c := range d.Status.Conditions {
			if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
				stuck = true
				stuckMsg = c.Message
			}
		}
		unavail := d.Status.UnavailableReplicas
		if !stuck && unavail == 0 && d.Status.AvailableReplicas >= want {
			continue
		}
		ref := d.Namespace + "/" + d.Name
		ev := []string{fmt.Sprintf("期望 %d，可用 %d，不可用 %d，已更新 %d", want, d.Status.AvailableReplicas, unavail, d.Status.UpdatedReplicas)}
		rev := analyzerRevisionOf(d)
		if stuck {
			ev = append(ev, "滚动更新超时（ProgressDeadlineExceeded）"+analyzerTrimMsg(stuckMsg))
		}
		var plan PlanDraft
		sev, conf, msg := sevWarning, confMedium, "Deployment 副本不可用"
		if stuck {
			sev, conf, msg = sevCritical, confHigh, "Deployment 滚动更新卡住（ProgressDeadlineExceeded）"
		}
		if stuck && rev > 1 {
			plan = PlanDraft{
				Title:       "回滚 Deployment " + ref + " 到上一版本",
				Description: fmt.Sprintf("第 %d 版滚动更新超时，回滚到上一版本并等待就绪，之后再排查新版本问题。", rev),
				Steps: []StepDraft{
					analyzerStep("k8s.events", analyzerObjParams("deployment", d.Namespace, d.Name), "确认卡住原因"),
					analyzerStep("k8s.rollback_deployment", analyzerObjParams("deployment", d.Namespace, d.Name), ""),
				},
				Verify: []StepDraft{analyzerWaitReady("deployment", d.Namespace, d.Name)},
			}
		} else {
			plan = PlanDraft{
				Title:       "排查 Deployment " + ref + " 副本不可用",
				Description: "查看 Deployment 与其 Pod 的状态和事件，定位不可用副本的原因。",
				Steps: []StepDraft{
					analyzerStep("k8s.describe", analyzerObjParams("deployment", d.Namespace, d.Name), ""),
					analyzerStep("k8s.events", analyzerObjParams("deployment", d.Namespace, d.Name), ""),
				},
			}
		}
		out = append(out, analyzerNewFinding(analyzerDeployment, sev, conf, "Deployment "+ref, msg, ev, plan))
	}
	return out
}

// 9. Frequent restarts (not already CrashLoopBackOff / OOMKilled).
func analyzeRestarts(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.Pods {
		p := &in.Pods[i]
		for _, cs := range analyzerContainerStatuses(p) {
			if cs.RestartCount < restartThreshold {
				continue
			}
			if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
				continue
			}
			if analyzerIsOOM(cs) {
				continue
			}
			ev := []string{fmt.Sprintf("容器 %s 已重启 %d 次（阈值 %d）", cs.Name, cs.RestartCount, restartThreshold)}
			if t := cs.LastTerminationState.Terminated; t != nil {
				ev = append(ev, fmt.Sprintf("上次退出码 %d（%s）", t.ExitCode, analyzerOrDash(t.Reason)))
			}
			plan := analyzerReadOnlyPodPlan(p, cs.Name, "排查 "+analyzerPodRef(p)+" 频繁重启", "查看上次退出前日志与事件，判断是应用错误、探针还是资源问题。")
			out = append(out, analyzerNewFinding(analyzerRestarts, sevWarning, confMedium, "Pod "+analyzerPodRef(p),
				fmt.Sprintf("容器 %s 频繁重启（%d 次）", cs.Name, cs.RestartCount), ev, plan))
		}
	}
	return out
}

// 10. PVC Pending.
func analyzePVCPending(in AnalyzerInput) []Finding {
	var out []Finding
	for i := range in.PVCs {
		c := &in.PVCs[i]
		if c.Status.Phase != corev1.ClaimPending {
			continue
		}
		sc := "<默认>"
		if c.Spec.StorageClassName != nil {
			sc = *c.Spec.StorageClassName
		}
		ev := []string{"PVC 处于 Pending，StorageClass：" + sc}
		ev = append(ev, analyzerEventLines(in, "PersistentVolumeClaim", c.Namespace, c.Name, "")...)
		plan := PlanDraft{
			Title:       "排查 PVC " + c.Namespace + "/" + c.Name + " 未绑定",
			Description: "检查 StorageClass 是否存在、供应器是否正常、是否有匹配的 PV（容量/访问模式）。",
			Steps: []StepDraft{
				analyzerStep("k8s.describe", analyzerObjParams("pvc", c.Namespace, c.Name), ""),
				analyzerStep("k8s.events", analyzerObjParams("pvc", c.Namespace, c.Name), "查看 ProvisioningFailed 等事件"),
			},
		}
		out = append(out, analyzerNewFinding(analyzerPVCPending, sevWarning, confHigh, "PersistentVolumeClaim "+c.Namespace+"/"+c.Name,
			"PVC 未绑定（Pending）", ev, plan))
	}
	return out
}

// ---------------------------------------------------------------- aggregate

// aggregateFindings sorts findings by confidence (then severity), keeps the
// first three distinct plans and returns a rules conclusion. With no findings
// it returns a read-only investigation plan. Callers set Case.Engine =
// rulesEngine (Conclusion has no Engine field).
func aggregateFindings(fs []Finding) (Conclusion, []PlanDraft) {
	sorted := append([]Finding(nil), fs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ci, cj := analyzerConfRank(findingConfidence(sorted[i])), analyzerConfRank(findingConfidence(sorted[j]))
		if ci != cj {
			return ci > cj
		}
		return analyzerSevRank(sorted[i].Severity) > analyzerSevRank(sorted[j].Severity)
	})
	if len(sorted) == 0 {
		return Conclusion{
				Summary:     "规则引擎未发现已知故障模式（Pod、节点、Deployment、PVC 均未命中规则）。",
				RootCause:   "不确定",
				Hypotheses:  []string{},
				Confidence:  confLow,
				Impact:      "未识别到受影响对象",
				MissingInfo: []string{"应用日志", "监控指标（CPU/内存/延迟）", "最近的变更记录"},
			}, []PlanDraft{{
				Title:       "只读排查",
				Description: "规则未命中，先查看命名空间事件与工作负载状态。",
				Steps: []StepDraft{
					analyzerStep("k8s.events", analyzerMustJSON(map[string]any{}), "查看最近的 Warning 事件"),
					analyzerStep("k8s.describe", analyzerMustJSON(map[string]any{"kind": "deployment"}), "查看工作负载状态"),
				},
			}}
	}

	var plans []PlanDraft
	seen := map[string]bool{}
	for _, f := range sorted {
		for _, p := range f.Plans {
			k := analyzerPlanKey(p)
			if seen[k] {
				continue
			}
			seen[k] = true
			if len(plans) < maxPlans {
				plans = append(plans, p)
			}
		}
	}

	top := sorted[0]
	conf := findingConfidence(top)
	c := Conclusion{
		Confidence: conf,
		Findings:   sorted,
		Hypotheses: []string{},
	}
	if conf == confHigh {
		c.RootCause = top.Object + "：" + top.Message
	} else {
		c.RootCause = "不确定"
		c.Hypotheses = append(c.Hypotheses, top.Object+"："+top.Message)
		c.MissingInfo = []string{"应用日志", "最近的变更记录"}
	}
	var objs []string
	seenObj := map[string]bool{}
	for _, f := range sorted[1:] {
		if len(c.Hypotheses) < 5 {
			c.Hypotheses = append(c.Hypotheses, f.Object+"："+f.Message)
		}
	}
	for _, f := range sorted {
		if !seenObj[f.Object] {
			seenObj[f.Object] = true
			objs = append(objs, f.Object)
		}
	}
	if len(objs) > 5 {
		objs = append(objs[:5], fmt.Sprintf("等共 %d 个对象", len(seenObj)))
	}
	c.Impact = "受影响对象：" + strings.Join(objs, "、")
	c.Summary = fmt.Sprintf("规则引擎发现 %d 个问题，最可能的是 %s：%s（置信度 %s）。", len(sorted), top.Object, top.Message, conf)
	return c, plans
}

func analyzerPlanKey(p PlanDraft) string {
	var b strings.Builder
	for _, s := range append(append([]StepDraft(nil), p.Steps...), p.Verify...) {
		b.WriteString(s.Action)
		b.WriteByte('|')
		b.Write(s.Params)
		b.WriteByte(';')
	}
	return b.String()
}

// findingConfidence reads the level stored as the first evidence line; falls
// back to severity (critical→medium, else low).
func findingConfidence(f Finding) string {
	for _, e := range f.Evidence {
		if strings.HasPrefix(e, confEvidencePrefix) {
			return strings.TrimPrefix(e, confEvidencePrefix)
		}
	}
	if f.Severity == sevCritical {
		return confMedium
	}
	return confLow
}

func analyzerConfRank(c string) int {
	switch c {
	case confHigh:
		return 3
	case confMedium:
		return 2
	case confLow:
		return 1
	}
	return 0
}

func analyzerSevRank(s string) int {
	switch s {
	case sevCritical:
		return 3
	case sevWarning:
		return 2
	case sevInfo:
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- helpers

func analyzerNewFinding(analyzer, sev, conf, object, msg string, ev []string, plan PlanDraft) Finding {
	if len(ev) > maxEvidence-1 {
		ev = ev[:maxEvidence-1]
	}
	return Finding{
		Analyzer: analyzer,
		Severity: sev,
		Object:   object,
		Message:  msg,
		Evidence: append([]string{confEvidencePrefix + conf}, ev...),
		Plans:    []PlanDraft{plan},
	}
}

func analyzerStep(action string, params json.RawMessage, note string) StepDraft {
	return StepDraft{Action: action, Params: params, Note: note}
}

func analyzerWaitReady(kind, ns, name string) StepDraft {
	return analyzerStep("k8s.wait_ready", analyzerObjParams(kind, ns, name), "等待就绪以验证恢复")
}

func analyzerObjParams(kind, ns, name string) json.RawMessage {
	m := map[string]any{"kind": kind, "name": name}
	if ns != "" {
		m["namespace"] = ns
	}
	return analyzerMustJSON(m)
}

func analyzerLogParams(p *corev1.Pod, container string, previous bool) json.RawMessage {
	return analyzerMustJSON(map[string]any{
		"namespace": p.Namespace, "name": p.Name, "container": container,
		"previous": previous, "tail_lines": 200,
	})
}

func analyzerReadOnlyPodPlan(p *corev1.Pod, container, title, desc string) PlanDraft {
	return PlanDraft{
		Title:       title,
		Description: desc,
		Steps: []StepDraft{
			analyzerStep("k8s.logs", analyzerLogParams(p, container, true), "上次退出前日志"),
			analyzerStep("k8s.describe", analyzerObjParams("pod", p.Namespace, p.Name), ""),
			analyzerStep("k8s.events", analyzerObjParams("pod", p.Namespace, p.Name), ""),
		},
	}
}

func analyzerMustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func analyzerPodRef(p *corev1.Pod) string { return p.Namespace + "/" + p.Name }

func analyzerContainerStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	out = append(out, p.Status.InitContainerStatuses...)
	return append(out, p.Status.ContainerStatuses...)
}

func analyzerIsOOM(cs corev1.ContainerStatus) bool {
	return (cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled") ||
		(cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled")
}

// workloadTarget resolves Pod -> ReplicaSet -> Deployment; falls back to the pod.
func analyzerWorkloadTarget(in AnalyzerInput, p *corev1.Pod) (kind, ns, name string) {
	for _, o := range p.OwnerReferences {
		if o.Kind != "ReplicaSet" {
			continue
		}
		for i := range in.ReplicaSets {
			rs := &in.ReplicaSets[i]
			if rs.Namespace == p.Namespace && rs.Name == o.Name {
				for _, ro := range rs.OwnerReferences {
					if ro.Kind == "Deployment" {
						return "deployment", p.Namespace, ro.Name
					}
				}
			}
		}
		if h := p.Labels["pod-template-hash"]; h != "" && strings.HasSuffix(o.Name, "-"+h) {
			d := strings.TrimSuffix(o.Name, "-"+h)
			for i := range in.Deployments {
				if in.Deployments[i].Namespace == p.Namespace && in.Deployments[i].Name == d {
					return "deployment", p.Namespace, d
				}
			}
		}
	}
	return "pod", p.Namespace, p.Name
}

func analyzerDeploymentRevision(in AnalyzerInput, ns, name string) int {
	for i := range in.Deployments {
		if in.Deployments[i].Namespace == ns && in.Deployments[i].Name == name {
			return analyzerRevisionOf(&in.Deployments[i])
		}
	}
	return 0
}

func analyzerRevisionOf(d *appsv1.Deployment) int {
	n, _ := strconv.Atoi(d.Annotations["deployment.kubernetes.io/revision"])
	return n
}

func analyzerMemoryLimit(p *corev1.Pod, container string) int64 {
	for _, c := range append(append([]corev1.Container(nil), p.Spec.InitContainers...), p.Spec.Containers...) {
		if c.Name == container {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				return q.Value()
			}
		}
	}
	return 0
}

const analyzerMiB = 1024 * 1024

func analyzerBumpMemory(b int64) string {
	return fmt.Sprintf("%dMi", (b*3/2+analyzerMiB-1)/analyzerMiB)
}

func analyzerFmtMi(b int64) string { return fmt.Sprintf("%dMi", (b+analyzerMiB-1)/analyzerMiB) }

func analyzerSpecImage(p *corev1.Pod, container string) string {
	for _, c := range append(append([]corev1.Container(nil), p.Spec.InitContainers...), p.Spec.Containers...) {
		if c.Name == container {
			return c.Image
		}
	}
	return ""
}

func analyzerFindPod(in AnalyzerInput, ns, name string) *corev1.Pod {
	for i := range in.Pods {
		if in.Pods[i].Namespace == ns && in.Pods[i].Name == name {
			return &in.Pods[i]
		}
	}
	return nil
}

func analyzerPvcClaims(p *corev1.Pod) []string {
	var out []string
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			out = append(out, v.PersistentVolumeClaim.ClaimName)
		}
	}
	return out
}

// podConfigRefs lists non-optional ConfigMap / Secret names a pod references.
func analyzerPodConfigRefs(p *corev1.Pod) (cms, secs []string) {
	add := func(list *[]string, n string, opt *bool) {
		if n == "" || (opt != nil && *opt) || analyzerContains(*list, n) {
			return
		}
		*list = append(*list, n)
	}
	for _, c := range append(append([]corev1.Container(nil), p.Spec.InitContainers...), p.Spec.Containers...) {
		for _, e := range c.EnvFrom {
			if e.ConfigMapRef != nil {
				add(&cms, e.ConfigMapRef.Name, e.ConfigMapRef.Optional)
			}
			if e.SecretRef != nil {
				add(&secs, e.SecretRef.Name, e.SecretRef.Optional)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.ConfigMapKeyRef; r != nil {
				add(&cms, r.Name, r.Optional)
			}
			if r := e.ValueFrom.SecretKeyRef; r != nil {
				add(&secs, r.Name, r.Optional)
			}
		}
	}
	for _, v := range p.Spec.Volumes {
		if v.ConfigMap != nil {
			add(&cms, v.ConfigMap.Name, v.ConfigMap.Optional)
		}
		if v.Secret != nil {
			add(&secs, v.Secret.SecretName, v.Secret.Optional)
		}
	}
	return cms, secs
}

func analyzerEventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

func analyzerSortedEvents(evs []corev1.Event) []corev1.Event {
	out := append([]corev1.Event(nil), evs...)
	sort.SliceStable(out, func(i, j int) bool { return analyzerEventTime(out[i]).Before(analyzerEventTime(out[j])) })
	return out
}

func analyzerPodEvents(in AnalyzerInput, ns, name string) []corev1.Event {
	var out []corev1.Event
	for _, e := range analyzerSortedEvents(in.Events) {
		if e.InvolvedObject.Kind == "Pod" && e.InvolvedObject.Namespace == ns && e.InvolvedObject.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// eventLines returns up to 3 recent event summaries for an object (reason
// filter optional).
func analyzerEventLines(in AnalyzerInput, kind, ns, name, reason string) []string {
	var out []string
	for _, e := range analyzerSortedEvents(in.Events) {
		o := e.InvolvedObject
		if o.Kind != kind || o.Namespace != ns || o.Name != name || (reason != "" && e.Reason != reason) {
			continue
		}
		out = append(out, "事件 "+e.Reason+"："+analyzerTruncate(e.Message, 200))
	}
	return analyzerLastN(out, 3)
}

func analyzerLastN(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func analyzerTruncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

func analyzerTrimMsg(m string) string {
	if m = analyzerTruncate(m, 200); m == "" {
		return ""
	}
	return "：" + m
}

func analyzerOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func analyzerContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
