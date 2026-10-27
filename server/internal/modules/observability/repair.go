package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
)

// Repair targets come exclusively from live Kubernetes owner references.
// Stateful components and node maintenance remain manual operations.
type repairTarget struct {
	ClusterID   int64  `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	Env         string `json:"env"`
	Namespace   string `json:"namespace"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	UID         string `json:"uid"`
	Generation  int64  `json:"generation"`
	Replicas    int32  `json:"replicas"`
	Ready       int32  `json:"ready"`
}

func (m *Module) repairCluster(ctx context.Context, id int64) (healthCluster, error) {
	var cl healthCluster
	err := m.d.DB.QueryRow(ctx, "SELECT id,name,env,api_server,kubeconfig FROM cluster_clusters WHERE id=$1", id).
		Scan(&cl.ID, &cl.Name, &cl.Env, &cl.API, &cl.EncryptedConfig)
	return cl, err
}

func (m *Module) resolveRepairTarget(ctx context.Context, a *Alert, evidence map[string]any, inspectHost bool) (repairTarget, error) {
	none := repairTarget{}
	ref := a.Labels["cluster_id"]
	if ref == "" {
		ref = a.Labels["cluster"]
	}
	if ref == "" {
		var options []byte
		if err := m.d.DB.QueryRow(ctx, "SELECT options FROM obs_datasources WHERE id=$1", a.DatasourceID).Scan(&options); err != nil {
			return none, err
		}
		var opts map[string]any
		_ = json.Unmarshal(options, &opts)
		if value, ok := opts["cluster_id"].(string); ok {
			ref = value
		} else if value, ok := opts["cluster_id"].(float64); ok {
			ref = strconv.FormatInt(int64(value), 10)
		}
	}
	if ref == "" {
		return none, errors.New("alert does not identify a cluster")
	}
	rows, err := m.d.DB.Query(ctx, "SELECT id,name,env,api_server,kubeconfig FROM cluster_clusters WHERE env=$1 AND (id::text=$2 OR name=$2)", a.Env, ref)
	if err != nil {
		return none, err
	}
	candidates := []healthCluster{}
	for rows.Next() {
		var cl healthCluster
		if rows.Scan(&cl.ID, &cl.Name, &cl.Env, &cl.API, &cl.EncryptedConfig) == nil {
			candidates = append(candidates, cl)
		}
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil || len(candidates) != 1 {
		return none, errors.New("cluster ownership is ambiguous or unavailable")
	}
	cl := candidates[0]
	cs, err := m.healthClient(ctx, cl)
	if err != nil {
		return none, err
	}
	if nodeName := a.Labels["node"]; nodeName != "" {
		evidence["node_diagnostics"] = nodeDiagnosisEvidence(ctx, cs, nodeName)
		evidence["host_agent_snapshot"] = hostAgentDiagnosisEvidence(ctx, a.Env, nodeName, cs, m.d.HostDiagnostics)
		if inspectHost {
			evidence["host_agent_diagnostics"] = hostAgentInspectionEvidence(ctx, a.Env, nodeName, cs, m.d.HostDiagnosticInspector)
		}
		return none, errors.New("node-level remediation remains manual; diagnosis is read-only")
	}
	namespace, name, uid := a.Labels["namespace"], a.Labels["deployment"], ""
	if namespace == "" || a.Labels["node"] != "" || a.Labels["statefulset"] != "" || a.Labels["daemonset"] != "" {
		return none, errors.New("only explicitly identified Deployments support this automated repair")
	}
	if podName := a.Labels["pod"]; podName != "" {
		pod, err := cs.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil || pod.DeletionTimestamp != nil {
			return none, errors.New("alert Pod no longer exists")
		}
		if a.Labels["kind"] == "Pod" && a.Labels["uid"] != "" && a.Labels["uid"] != string(pod.UID) {
			return none, errors.New("alert Pod was recreated")
		}
		evidence["pod_status"] = map[string]any{
			"name": pod.Name, "namespace": namespace, "phase": pod.Status.Phase, "node": pod.Spec.NodeName,
			"containers": containerDiagnosisEvidence(*pod),
		}
		evidence["host_agent_snapshot"] = hostAgentDiagnosisEvidence(ctx, a.Env, pod.Spec.NodeName, cs, m.d.HostDiagnostics)
		if inspectHost {
			evidence["host_agent_diagnostics"] = hostAgentInspectionEvidence(ctx, a.Env, pod.Spec.NodeName, cs, m.d.HostDiagnosticInspector)
		}
		evidence["container_logs"] = containerDiagnosisLogEvidence(ctx, cs, *pod)
		events, eventErr := cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fields.AndSelectors(
				fields.OneTermEqualSelector("involvedObject.name", pod.Name),
				fields.OneTermEqualSelector("type", corev1.EventTypeWarning),
			).String(),
			Limit: 100,
		})
		if eventErr != nil {
			evidence["pod_warning_events"] = "unavailable"
		} else {
			evidence["pod_warning_events"] = warningEventDiagnosisEvidence(events.Items, *pod)
		}
		var owner *metav1.OwnerReference
		for i := range pod.OwnerReferences {
			if pod.OwnerReferences[i].Controller != nil && *pod.OwnerReferences[i].Controller {
				if owner != nil {
					return none, errors.New("multiple Pod controllers")
				}
				owner = &pod.OwnerReferences[i]
			}
		}
		if owner == nil || owner.Kind != "ReplicaSet" {
			return none, errors.New("Pod has no Deployment controller")
		}
		rs, err := cs.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil || rs.UID != owner.UID {
			return none, errors.New("ReplicaSet ownership changed")
		}
		found := ""
		for _, o := range rs.OwnerReferences {
			if o.Controller != nil && *o.Controller && o.Kind == "Deployment" {
				if found != "" {
					return none, errors.New("ambiguous Deployment ownership")
				}
				found, uid = o.Name, string(o.UID)
			}
		}
		if found == "" || (name != "" && name != found) {
			return none, errors.New("Deployment labels and live owner references disagree")
		}
		name = found
	}
	if name == "" {
		return none, errors.New("alert does not identify a Deployment")
	}
	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil || dep.DeletionTimestamp != nil {
		return none, errors.New("Deployment unavailable")
	}
	if uid != "" && string(dep.UID) != uid {
		return none, errors.New("Deployment UID changed")
	}
	if a.Labels["kind"] == "Deployment" && a.Labels["uid"] != "" && a.Labels["uid"] != string(dep.UID) {
		return none, errors.New("alert Deployment was recreated")
	}
	evidence["deployment_status"] = deploymentDiagnosisEvidence(dep)
	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	target := repairTarget{cl.ID, cl.Name, cl.Env, namespace, "deployments", dep.Name, string(dep.UID), dep.Generation, replicas, dep.Status.AvailableReplicas}
	evidence["controller"] = target
	if replicas == 0 || dep.Spec.Paused || (dep.Spec.Strategy.Type != "" && dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType) {
		return none, errors.New("scaled-down, paused or Recreate workloads require manual handling")
	}
	return target, nil
}

type remediationHandler struct{ m *Module }

func (h *remediationHandler) OnApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var payload struct {
		DiagnosisID int64 `json:"diagnosis_id"`
	}
	if err := t.DecodePayload(&payload); err != nil || payload.DiagnosisID < 1 {
		return nil, httpx.Validation("invalid diagnosis approval")
	}
	p, err := h.m.repairPrincipal(ctx, t.ApplicantID, t.ApplicantName, t.Env)
	if err != nil {
		return nil, err
	}
	tag, err := h.m.d.DB.Exec(ctx, `UPDATE obs_diagnoses SET status='approved',updated_at=now()
		WHERE id=$1 AND status='awaiting_approval' AND decision_by=$2 AND approval_id=$3
		AND EXISTS(SELECT 1 FROM obs_alerts a JOIN obs_datasources ds ON ds.id=a.datasource_id
		 WHERE a.id=obs_diagnoses.alert_id AND a.fire_count=obs_diagnoses.occurrence AND a.status='firing' AND ds.env=$4)`,
		payload.DiagnosisID, p.UserID, t.ID, t.Env)
	if err == nil && tag.RowsAffected() == 0 {
		err = httpx.Conflict("diagnosis, applicant or alert changed")
	}
	return gin.H{"diagnosis_id": payload.DiagnosisID, "status": "approved"}, err
}

func (h *remediationHandler) OnRejected(ctx context.Context, t *approval.Ticket) error {
	var payload struct {
		DiagnosisID int64 `json:"diagnosis_id"`
	}
	if err := t.DecodePayload(&payload); err != nil {
		return err
	}
	_, err := h.m.d.DB.Exec(ctx, `UPDATE obs_diagnoses SET status='manual',error='environment approval rejected or withdrawn',updated_at=now()
		WHERE id=$1 AND decision_by=$2 AND approval_id=$3 AND status='awaiting_approval'`, payload.DiagnosisID, t.ApplicantID, t.ID)
	return err
}

func (m *Module) repairPrincipal(ctx context.Context, id int64, name, env string) (*rbac.Principal, error) {
	if m.d.RBAC == nil || id < 1 {
		return nil, httpx.Forbidden("repair principal unavailable")
	}
	_, permissions, scope, err := m.d.RBAC.UserAccess(ctx, id)
	if err != nil {
		return nil, err
	}
	p := &rbac.Principal{UserID: id, Username: name, Permissions: permissions, EnvScope: scope}
	if !p.Can("workload:write") || !p.Can("cluster:read") || !p.Can("ai:chat") || !p.Can(PermAlertHandle) || !p.CanEnv(env) {
		return nil, httpx.Forbidden("repair permissions or environment scope revoked")
	}
	return p, nil
}

func (m *Module) executeRepair(ctx context.Context, d *Diagnosis) {
	ctx, cancel := context.WithTimeout(ctx, 7*time.Minute)
	defer cancel()
	var target repairTarget
	var report analysis
	_ = json.Unmarshal(d.Target, &target)
	_ = json.Unmarshal(d.Analysis, &report)
	var userID int64
	if d.DecisionBy != nil {
		userID = *d.DecisionBy
	}
	_, err := m.repairPrincipal(ctx, userID, d.DecisionName, target.Env)
	a, loadErr := scanAlert(m.d.DB.QueryRow(ctx, "SELECT "+alertCols+" FROM obs_alerts a JOIN obs_datasources d ON d.id=a.datasource_id WHERE a.id=$1", d.AlertID))
	if err == nil {
		err = loadErr
	}
	if err == nil && (a.Status != "firing" || a.FireCount != d.Occurrence || a.Env != target.Env || a.HandleStatus == "closed") {
		err = errors.New("alert changed")
	}
	if err == nil && (target.Kind != "deployments" || target.UID == "" || report.ProposedAction != "restart_workload") {
		err = errors.New("repair action is not supported")
	}
	var cl healthCluster
	if err == nil {
		cl, err = m.repairCluster(ctx, target.ClusterID)
		if err == nil && cl.Env != target.Env {
			err = errors.New("cluster environment changed")
		}
	}
	if err == nil {
		var dep *appsv1.Deployment
		cs, clientErr := m.healthClient(ctx, cl)
		err = clientErr
		if err == nil {
			dep, err = cs.AppsV1().Deployments(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		}
		if err == nil && (string(dep.UID) != target.UID || dep.Generation != target.Generation || dep.Spec.Paused ||
			dep.DeletionTimestamp != nil || (dep.Spec.Strategy.Type != "" && dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType)) {
			err = errors.New("controller identity or configuration changed; analyze again")
		}
		if err == nil {
			annotations := map[string]string{}
			for k, v := range dep.Spec.Template.Annotations {
				annotations[k] = v
			}
			annotations["matrix.aiops/restarted-at"] = time.Now().UTC().Format(time.RFC3339Nano)
			annotations["matrix.aiops/diagnosis-id"] = fmt.Sprint(d.ID)
			patch, _ := json.Marshal([]map[string]any{
				{"op": "test", "path": "/metadata/uid", "value": target.UID},
				{"op": "test", "path": "/metadata/resourceVersion", "value": dep.ResourceVersion},
				{"op": "add", "path": "/spec/template/metadata/annotations", "value": annotations},
			})
			dep, err = cs.AppsV1().Deployments(target.Namespace).Patch(ctx, target.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
			if err == nil {
				generation := dep.Generation
				tick := time.NewTicker(5 * time.Second)
				defer tick.Stop()
				for {
					current, getErr := cs.AppsV1().Deployments(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
					if getErr != nil {
						err = getErr
						break
					}
					if current.UID != dep.UID || current.Generation != generation {
						err = errors.New("workload changed during rollout verification")
						break
					}
					desired := int32(1)
					if current.Spec.Replicas != nil {
						desired = *current.Spec.Replicas
					}
					if current.Status.ObservedGeneration >= generation && current.Status.UpdatedReplicas == desired &&
						current.Status.AvailableReplicas == desired && current.Status.Replicas == desired {
						break
					}
					select {
					case <-ctx.Done():
						err = ctx.Err()
					case <-tick.C:
					}
					if err != nil {
						break
					}
				}
			}
		}
	}
	if err != nil {
		m.setDiagnosisFailed(d.ID, "受控处置未完成：资源身份、权限、告警或就绪状态发生变化；请人工检查。平台不会自动重试重启。", "executing")
	} else {
		saveCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = m.d.DB.Exec(saveCtx, "UPDATE obs_diagnoses SET status='succeeded',error='',updated_at=now() WHERE id=$1 AND status='executing'", d.ID)
		stop()
	}
	if m.d.Audit != nil {
		result := audit.ResultSuccess
		if err != nil {
			result = audit.ResultFailure
		}
		id := userID
		m.d.Audit.Record(context.Background(), audit.Entry{UserID: &id, Username: d.DecisionName, Action: "aiops.restart_workload",
			Resource: "obs_diagnosis", ResourceID: fmt.Sprint(d.ID), Result: result,
			Detail: json.RawMessage(fmt.Sprintf(`{"cluster_id":%d,"diagnosis_id":%d}`, target.ClusterID, d.ID))})
	}
	if a != nil {
		m.notifyAlerts(context.Background(), []Alert{*a})
	}
}
