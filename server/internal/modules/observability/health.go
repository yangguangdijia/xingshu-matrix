package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type healthCluster struct {
	ID              int64
	Name, Env, API  string
	EncryptedConfig string
}

func (m *Module) healthClient(ctx context.Context, cl healthCluster) (kubernetes.Interface, error) {
	raw, err := m.d.Crypto.Decrypt(cl.EncryptedConfig)
	if err != nil {
		return nil, errors.New("cluster credentials unavailable")
	}
	kc, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return nil, errors.New("invalid cluster credentials")
	}
	for _, a := range kc.AuthInfos {
		if a.Exec != nil || a.AuthProvider != nil || a.TokenFile != "" || a.ClientCertificate != "" || a.ClientKey != "" {
			return nil, errors.New("health checks require embedded credentials without exec plugins or local files")
		}
	}
	for _, c := range kc.Clusters {
		if c.CertificateAuthority != "" {
			return nil, errors.New("health checks require embedded CA data")
		}
	}
	cfg, err := clientcmd.NewDefaultClientConfig(*kc, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, errors.New("invalid cluster config")
	}
	cfg.Timeout = 12 * time.Second
	return kubernetes.NewForConfig(cfg)
}

type healthFinding struct {
	Kind, Namespace, Name, UID, Reason, Severity, Summary string
}

func nodeFindings(n corev1.Node, now time.Time) []healthFinding {
	items := []healthFinding{}
	if now.Sub(n.CreationTimestamp.Time) < 2*time.Minute {
		return items
	}
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
		if (c.Type == corev1.NodeMemoryPressure || c.Type == corev1.NodeDiskPressure || c.Type == corev1.NodePIDPressure) && c.Status == corev1.ConditionTrue {
			items = append(items, healthFinding{"Node", "", n.Name, string(n.UID), string(c.Type), "warning", safeEvidence(c.Message, 800)})
		}
	}
	if !ready {
		items = append(items, healthFinding{"Node", "", n.Name, string(n.UID), "NodeNotReady", "critical", "节点未就绪；可能影响其上 Pod 调度和现有应用，请检查 kubelet、网络和主机状态"})
	}
	return items
}

func podFinding(p corev1.Pod, now time.Time) *healthFinding {
	if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || now.Sub(p.CreationTimestamp.Time) < 2*time.Minute {
		return nil
	}
	reason, summary := "", ""
	for _, c := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if c.State.Waiting != nil {
			switch c.State.Waiting.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "RunContainerError":
				reason, summary = c.State.Waiting.Reason, c.State.Waiting.Message
			}
		}
		if c.State.Terminated != nil && c.State.Terminated.Reason == "OOMKilled" && now.Sub(c.State.Terminated.FinishedAt.Time) < 10*time.Minute {
			reason, summary = "OOMKilled", "容器因内存不足被终止；需核对内存限制、工作集及节点压力，重启不保证解决根因"
		}
	}
	if reason == "" && p.Status.Phase == corev1.PodPending && now.Sub(p.CreationTimestamp.Time) >= 5*time.Minute {
		reason, summary = "PodPending", "Pod 超过 5 分钟未进入运行状态；检查调度事件、资源配额、存储和网络"
	}
	if reason == "" && p.Status.Phase == corev1.PodFailed {
		reason, summary = "PodFailed", p.Status.Message
	}
	if reason == "" {
		return nil
	}
	return &healthFinding{"Pod", p.Namespace, p.Name, string(p.UID), reason, "warning", safeEvidence(summary, 800)}
}

func deploymentFinding(d appsv1.Deployment, now time.Time) *healthFinding {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	if desired == 0 || d.DeletionTimestamp != nil || now.Sub(d.CreationTimestamp.Time) < 5*time.Minute {
		return nil
	}
	for _, c := range d.Status.Conditions {
		if (c.Type == appsv1.DeploymentAvailable || c.Type == appsv1.DeploymentProgressing) && c.Status == corev1.ConditionFalse &&
			now.Sub(c.LastTransitionTime.Time) >= 2*time.Minute {
			return &healthFinding{"Deployment", d.Namespace, d.Name, string(d.UID), "DeploymentUnavailable", "critical",
				fmt.Sprintf("期望 %d 副本、当前可用 %d；%s", desired, d.Status.AvailableReplicas, safeEvidence(c.Message, 700))}
		}
	}
	return nil
}

func (m *Module) healthSource(ctx context.Context, cl healthCluster) (int64, error) {
	opts, _ := json.Marshal(map[string]any{"cluster_id": strconv.FormatInt(cl.ID, 10), "managed_by": "aiops"})
	var id int64
	err := m.d.DB.QueryRow(ctx, `INSERT INTO obs_datasources(name,type,url,env,options,created_by)
		VALUES($1,'kubernetes',$2,$3,$4,'aiops')
		ON CONFLICT(name) DO UPDATE SET url=EXCLUDED.url,env=EXCLUDED.env,updated_at=now()
		WHERE obs_datasources.type='kubernetes' AND obs_datasources.options->>'managed_by'='aiops'
		RETURNING id`, fmt.Sprintf("Matrix Kubernetes #%d", cl.ID), cl.API, cl.Env, opts).Scan(&id)
	return id, err
}

func findingLabels(cl healthCluster, f healthFinding) map[string]string {
	labels := map[string]string{"cluster_id": fmt.Sprint(cl.ID), "cluster": cl.Name, "kind": f.Kind, "name": f.Name, "reason": f.Reason, "uid": f.UID}
	if f.Namespace != "" {
		labels["namespace"] = f.Namespace
	}
	switch f.Kind {
	case "Pod":
		labels["pod"] = f.Name
	case "Node":
		labels["node"] = f.Name
	case "Deployment":
		labels["deployment"] = f.Name
	case "StatefulSet":
		labels["statefulset"] = f.Name
	case "DaemonSet":
		labels["daemonset"] = f.Name
	}
	return labels
}

func (m *Module) recordFinding(ctx context.Context, cl healthCluster, source int64, f healthFinding) (*Alert, error) {
	labels := findingLabels(cl, f)
	fingerprint := labelFingerprint(labels)
	lb, _ := json.Marshal(labels)
	an, _ := json.Marshal(map[string]string{"summary": f.Reason, "description": f.Summary})
	var id int64
	var fireCount int
	var prior string
	err := m.d.DB.QueryRow(ctx, `WITH prev AS(SELECT status FROM obs_alerts WHERE datasource_id=$1 AND fingerprint=$2),
		up AS(INSERT INTO obs_alerts(datasource_id,fingerprint,alertname,severity,status,labels,annotations,starts_at)
		VALUES($1,$2,$3,$4,'firing',$5,$6,now())
		ON CONFLICT(datasource_id,fingerprint) DO UPDATE SET
		 annotations=EXCLUDED.annotations,last_received_at=now(),status='firing',ends_at=NULL,
		 starts_at=CASE WHEN obs_alerts.status='resolved' THEN now() ELSE obs_alerts.starts_at END,
		 fire_count=obs_alerts.fire_count+CASE WHEN obs_alerts.status='resolved' THEN 1 ELSE 0 END,
		 handle_status=CASE WHEN obs_alerts.status='resolved' THEN 'open' ELSE obs_alerts.handle_status END,
		 acked_by=CASE WHEN obs_alerts.status='resolved' THEN '' ELSE obs_alerts.acked_by END
		RETURNING id,fire_count)
		SELECT up.id,up.fire_count,COALESCE((SELECT status FROM prev),'') FROM up`,
		source, fingerprint, f.Reason, f.Severity, lb, an).Scan(&id, &fireCount, &prior)
	if err != nil || prior == "firing" {
		return nil, err
	}
	var annotations map[string]string
	_ = json.Unmarshal(an, &annotations)
	now := time.Now()
	return &Alert{ID: id, DatasourceID: source, Datasource: fmt.Sprintf("Matrix Kubernetes #%d", cl.ID), Env: cl.Env,
		Fingerprint: fingerprint, Alertname: f.Reason, Severity: f.Severity, Status: "firing", HandleStatus: "open",
		Labels: labels, Annotations: annotations, StartsAt: &now, FireCount: fireCount}, nil
}

func (m *Module) scanHealth(ctx context.Context, policy aiopsPolicy) {
	conn, err := m.d.DB.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	var leader bool
	if conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(82178065)").Scan(&leader) != nil || !leader {
		return
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(82178065)")
	}()
	rows, err := m.d.DB.Query(ctx, "SELECT id,name,env,api_server,kubeconfig FROM cluster_clusters ORDER BY id")
	if err != nil {
		return
	}
	clusters := []healthCluster{}
	for rows.Next() {
		var cl healthCluster
		if rows.Scan(&cl.ID, &cl.Name, &cl.Env, &cl.API, &cl.EncryptedConfig) == nil {
			clusters = append(clusters, cl)
		}
	}
	rows.Close()
	for _, cl := range clusters {
		if ctx.Err() != nil {
			return
		}
		m.scanClusterHealth(ctx, cl, policy)
	}
}

func (m *Module) scanClusterHealth(parent context.Context, cl healthCluster, policy aiopsPolicy) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	source, err := m.healthSource(ctx, cl)
	if err != nil {
		m.logWarn("health source", err)
		return
	}
	cs, err := m.healthClient(ctx, cl)
	findings := []healthFinding{}
	complete := false
	if err == nil {
		// A bounded scan never marks unseen resources recovered.
		var nodes *corev1.NodeList
		var pods *corev1.PodList
		var deployments *appsv1.DeploymentList
		var statefulsets *appsv1.StatefulSetList
		var daemonsets *appsv1.DaemonSetList
		var volumes *corev1.PersistentVolumeClaimList
		nodes, err = cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 5000})
		if err == nil {
			pods, err = cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{Limit: 5000})
		}
		if err == nil {
			deployments, err = cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{Limit: 5000})
		}
		if err == nil {
			statefulsets, err = cs.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{Limit: 5000})
		}
		if err == nil {
			daemonsets, err = cs.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{Limit: 5000})
		}
		if err == nil {
			volumes, err = cs.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{Limit: 5000})
		}
		if err == nil {
			complete = nodes.Continue == "" && pods.Continue == "" && deployments.Continue == "" &&
				statefulsets.Continue == "" && daemonsets.Continue == "" && volumes.Continue == ""
			now := time.Now()
			for _, n := range nodes.Items {
				findings = append(findings, nodeFindings(n, now)...)
			}
			for _, p := range pods.Items {
				if f := podFinding(p, now); f != nil {
					findings = append(findings, *f)
				}
			}
			for _, d := range deployments.Items {
				if f := deploymentFinding(d, now); f != nil {
					findings = append(findings, *f)
				}
			}
			for _, s := range statefulsets.Items {
				desired := int32(1)
				if s.Spec.Replicas != nil {
					desired = *s.Spec.Replicas
				}
				if s.DeletionTimestamp == nil && now.Sub(s.CreationTimestamp.Time) > 5*time.Minute && s.Status.ReadyReplicas < desired {
					findings = append(findings, healthFinding{"StatefulSet", s.Namespace, s.Name, string(s.UID), "StatefulSetNotReady", "warning",
						fmt.Sprintf("就绪副本 %d/%d；有状态服务重启可能影响仲裁和持久化，请人工评估", s.Status.ReadyReplicas, desired)})
				}
			}
			for _, d := range daemonsets.Items {
				if d.DeletionTimestamp == nil && now.Sub(d.CreationTimestamp.Time) > 5*time.Minute && d.Status.NumberReady < d.Status.DesiredNumberScheduled {
					findings = append(findings, healthFinding{"DaemonSet", d.Namespace, d.Name, string(d.UID), "DaemonSetNotReady", "warning",
						fmt.Sprintf("就绪节点 %d/%d；可能影响节点级网络、日志或系统组件", d.Status.NumberReady, d.Status.DesiredNumberScheduled)})
				}
			}
			for _, v := range volumes.Items {
				if v.DeletionTimestamp == nil && now.Sub(v.CreationTimestamp.Time) > 5*time.Minute && v.Status.Phase == corev1.ClaimPending {
					findings = append(findings, healthFinding{"PersistentVolumeClaim", v.Namespace, v.Name, string(v.UID), "VolumePending", "warning",
						"PVC 超过 5 分钟未绑定；检查 StorageClass、容量和消费 Pod 的调度条件，WaitForFirstConsumer 也可能延后绑定"})
				}
			}
		}
	}
	if err != nil {
		// Do not expose kubeconfig, certificates, or raw upstream responses.
		findings = append(findings, healthFinding{"Cluster", "", cl.Name, "", "ClusterHealthCheckFailed", "critical",
			"平台无法完成集群健康巡检；可能是 API 连接、凭据或资源读取权限问题，不能据此断言整个集群宕机"})
	}
	active := []string{}
	alerts := []Alert{}
	for _, f := range findings {
		a, err := m.recordFinding(ctx, cl, source, f)
		if err != nil {
			return // A partial database write must never resolve other alerts.
		}
		if a != nil {
			alerts = append(alerts, *a)
			active = append(active, a.Fingerprint)
		} else {
			active = append(active, labelFingerprint(findingLabels(cl, f)))
		}
	}
	if complete {
		_, _ = m.d.DB.Exec(ctx, `UPDATE obs_alerts SET status='resolved',ends_at=now(),last_received_at=now()
			WHERE datasource_id=$1 AND status='firing' AND NOT(fingerprint=ANY($2::text[]))`, source, active)
	}
	if len(alerts) > 0 {
		m.notifyAlerts(ctx, alerts)
	}
}
