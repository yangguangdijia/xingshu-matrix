package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

const fieldManager = "xingshu-matrix"

// workloadGVR maps supported kinds (plural, lowercase) to resources.
var workloadGVR = map[string]schema.GroupVersionResource{
	"deployments":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"statefulsets": {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"daemonsets":   {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"jobs":         {Group: "batch", Version: "v1", Resource: "jobs"},
	"cronjobs":     {Group: "batch", Version: "v1", Resource: "cronjobs"},
	"pods":         {Group: "", Version: "v1", Resource: "pods"},
}

var kindAliases = map[string]string{
	"deployment": "deployments", "deploy": "deployments",
	"statefulset": "statefulsets", "sts": "statefulsets",
	"daemonset": "daemonsets", "ds": "daemonsets",
	"job": "jobs", "cronjob": "cronjobs", "cj": "cronjobs",
	"pod": "pods", "po": "pods",
}

func normalizeKind(k string) (string, error) {
	k = strings.ToLower(k)
	if a, ok := kindAliases[k]; ok {
		k = a
	}
	if _, ok := workloadGVR[k]; !ok {
		return "", httpx.BadRequest("unsupported kind " + k + " (deployments|statefulsets|daemonsets|jobs|cronjobs|pods)")
	}
	return k, nil
}

type drainParams struct {
	GracePeriodSeconds *int64 `json:"grace_period_seconds"`
	IgnoreDaemonSets   *bool  `json:"ignore_daemonsets"`
	DeleteEmptyDirData bool   `json:"delete_emptydir_data"`
	Force              bool   `json:"force"` // also evict pods without a controller
}

type labelsParams struct {
	Labels map[string]*string `json:"labels"` // null value removes the key
}

type taintsParams struct {
	Taints []corev1.Taint `json:"taints"`
}

type nsParams struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type scaleParams struct {
	Replicas *int32 `json:"replicas"`
}

type deleteParams struct {
	GracePeriodSeconds *int64 `json:"grace_period_seconds"`
}

type applyParams struct {
	YAML      string `json:"yaml"`
	Namespace string `json:"namespace"` // default for namespaced objects without one
	DryRun    bool   `json:"dry_run"`
	Force     *bool  `json:"force"` // force field conflicts (default true)
}

type networkPolicyParams struct {
	UID             types.UID                       `json:"uid,omitempty"`
	ResourceVersion string                          `json:"resource_version,omitempty"`
	Labels          map[string]string               `json:"labels,omitempty"`
	Spec            *networkingv1.NetworkPolicySpec `json:"spec,omitempty"`
}

func validateResourcePreconditions(raw json.RawMessage, resource string) error {
	var p struct {
		UID             types.UID `json:"uid"`
		ResourceVersion string    `json:"resource_version"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return err
	}
	if p.UID == "" || p.ResourceVersion == "" {
		return httpx.BadRequest(resource + " delete requires uid and resource_version")
	}
	return nil
}

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return httpx.BadRequest("invalid params: " + err.Error())
	}
	return nil
}

func decodeNetworkPolicyParams(raw json.RawMessage, p *networkPolicyParams) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(p); err != nil {
		return httpx.BadRequest("invalid NetworkPolicy params: " + err.Error())
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return httpx.BadRequest("invalid NetworkPolicy params: trailing JSON data")
	}
	return nil
}

func validateNetworkPolicyChange(pl *changePayload) error {
	if errs := validation.IsDNS1123Label(pl.Namespace); len(errs) > 0 {
		return httpx.BadRequest("invalid NetworkPolicy namespace")
	}
	if errs := validation.IsDNS1123Subdomain(pl.Name); len(errs) > 0 {
		return httpx.BadRequest("invalid NetworkPolicy name")
	}

	var p networkPolicyParams
	if err := decodeNetworkPolicyParams(pl.Params, &p); err != nil {
		return err
	}
	switch pl.Action {
	case "network_policy_create":
		if p.UID != "" || p.ResourceVersion != "" {
			return httpx.BadRequest("create must not include uid or resource_version")
		}
		if p.Spec == nil {
			return httpx.BadRequest("spec is required")
		}
	case "network_policy_update":
		if p.UID == "" || p.ResourceVersion == "" {
			return httpx.BadRequest("uid and resource_version are required for update")
		}
		if p.Spec == nil {
			return httpx.BadRequest("spec is required")
		}
	case "network_policy_delete":
		if p.UID == "" || p.ResourceVersion == "" {
			return httpx.BadRequest("uid and resource_version are required for delete")
		}
		if p.Spec != nil || len(p.Labels) > 0 {
			return httpx.BadRequest("delete accepts only uid and resource_version")
		}
		return nil
	}

	for key, value := range p.Labels {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return httpx.BadRequest("invalid NetworkPolicy label key " + key)
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return httpx.BadRequest("invalid NetworkPolicy label value for " + key)
		}
	}
	spec := p.Spec
	if _, err := metav1.LabelSelectorAsSelector(&spec.PodSelector); err != nil {
		return httpx.BadRequest("invalid NetworkPolicy podSelector: " + err.Error())
	}
	seen := map[networkingv1.PolicyType]bool{}
	for _, policyType := range spec.PolicyTypes {
		if policyType != networkingv1.PolicyTypeIngress && policyType != networkingv1.PolicyTypeEgress {
			return httpx.BadRequest("policyTypes must contain only Ingress or Egress")
		}
		if seen[policyType] {
			return httpx.BadRequest("policyTypes must not contain duplicates")
		}
		seen[policyType] = true
	}
	if len(spec.PolicyTypes) == 0 {
		spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		seen[networkingv1.PolicyTypeIngress] = true
		if len(spec.Egress) > 0 {
			spec.PolicyTypes = append(spec.PolicyTypes, networkingv1.PolicyTypeEgress)
			seen[networkingv1.PolicyTypeEgress] = true
		}
	}
	if len(spec.Ingress) > 0 && !seen[networkingv1.PolicyTypeIngress] {
		return httpx.BadRequest("policyTypes must include Ingress when ingress rules are set")
	}
	if len(spec.Egress) > 0 && !seen[networkingv1.PolicyTypeEgress] {
		return httpx.BadRequest("policyTypes must include Egress when egress rules are set")
	}
	for _, rule := range spec.Ingress {
		if err := validateNetworkPolicyPeers(rule.From); err != nil {
			return httpx.BadRequest("invalid NetworkPolicy ingress peer: " + err.Error())
		}
	}
	for _, rule := range spec.Egress {
		if err := validateNetworkPolicyPeers(rule.To); err != nil {
			return httpx.BadRequest("invalid NetworkPolicy egress peer: " + err.Error())
		}
	}
	p.Spec = spec
	normalized, err := json.Marshal(p)
	if err != nil {
		return httpx.BadRequest("invalid NetworkPolicy params")
	}
	pl.Params = normalized
	return nil
}

func validateNetworkPolicyPeers(peers []networkingv1.NetworkPolicyPeer) error {
	for _, peer := range peers {
		if peer.PodSelector != nil {
			if _, err := metav1.LabelSelectorAsSelector(peer.PodSelector); err != nil {
				return err
			}
		}
		if peer.NamespaceSelector != nil {
			if _, err := metav1.LabelSelectorAsSelector(peer.NamespaceSelector); err != nil {
				return err
			}
		}
		if peer.IPBlock != nil {
			if _, _, err := net.ParseCIDR(peer.IPBlock.CIDR); err != nil {
				return fmt.Errorf("invalid CIDR %q", peer.IPBlock.CIDR)
			}
		}
	}
	return nil
}

// validateChange normalises and checks a payload before gating so invalid
// requests never produce an approval ticket.
func validateChange(pl *changePayload) error {
	switch pl.Action {
	case "cordon", "uncordon", "drain":
		return decodeParams(pl.Params, &drainParams{})
	case "node_labels":
		var p labelsParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if len(p.Labels) == 0 {
			return httpx.BadRequest("labels is required")
		}
	case "node_taints":
		return decodeParams(pl.Params, &taintsParams{})
	case "node_batch_labels":
		var p nodeBatchLabelsParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if err := validateNodeBatchLabels(&p); err != nil {
			return err
		}
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "node_batch_taints":
		var p nodeBatchTaintsParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if err := validateNodeBatchTaints(&p); err != nil {
			return err
		}
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "ns_create":
		var p nsParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if p.Name == "" {
			return httpx.BadRequest("name is required")
		}
		pl.Name = p.Name
	case "ns_delete":
		if pl.Namespace == "" {
			return httpx.BadRequest("namespace is required")
		}
	case "scale", "restart", "delete":
		k, err := normalizeKind(pl.Kind)
		if err != nil {
			return err
		}
		pl.Kind = k
		if pl.Action == "scale" {
			var p scaleParams
			if err := decodeParams(pl.Params, &p); err != nil {
				return err
			}
			if p.Replicas == nil || *p.Replicas < 0 {
				return httpx.BadRequest("replicas (>=0) is required")
			}
			if k != "deployments" && k != "statefulsets" {
				return httpx.BadRequest("scale supports deployments and statefulsets")
			}
		}
		if pl.Action == "restart" && k != "deployments" && k != "statefulsets" && k != "daemonsets" {
			return httpx.BadRequest("restart supports deployments, statefulsets and daemonsets")
		}
	case "apply":
		var p applyParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if strings.TrimSpace(p.YAML) == "" {
			return httpx.BadRequest("yaml is required")
		}
		if err := validateApplyYAML(p.YAML); err != nil {
			return err
		}
	case "network_policy_create", "network_policy_update", "network_policy_delete":
		return validateNetworkPolicyChange(pl)
	case "sc_create", "sc_update", "sc_delete", "pvc_create", "pvc_update", "pvc_delete",
		"pvc_batch_delete", "pv_create", "pv_update", "pv_delete", "snapshot_create", "snapshot_delete":
		return validateStorageChange(pl)
	case "service_create", "service_update", "service_delete",
		"ingress_create", "ingress_update", "ingress_delete", "ingress_annotations":
		return validateNetworkResourceChange(pl)
	case "configmap_create", "configmap_update", "configmap_delete", "configmap_batch_delete",
		"secret_create", "secret_update", "secret_delete", "secret_batch_delete":
		return validateConfigChange(pl)
	case "hpa_create", "hpa_update", "hpa_delete",
		"cronjob_create", "cronjob_update", "cronjob_delete", "cronjob_run":
		return validateAutoscalingCronChange(pl)
	default:
		return httpx.BadRequest("unknown action " + pl.Action)
	}
	return nil
}

func validateApplyYAML(yaml string) error {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(yaml)), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return httpx.BadRequest("yaml decode: " + err.Error())
		}
		if len(obj.Object) == 0 {
			continue
		}
		gvk := obj.GroupVersionKind()
		if gvk.Group == "networking.k8s.io" && gvk.Kind == "NetworkPolicy" {
			return httpx.Forbidden("manage NetworkPolicy through the dedicated network policy API")
		}
		if err := rejectSensitiveApplyKind(gvk); err != nil {
			return err
		}
	}
}

// rejectSensitiveApplyKind keeps the generic endpoint limited to the workload
// types supported by the UI; all other resources use their dedicated APIs.
func rejectSensitiveApplyKind(gvk schema.GroupVersionKind) error {
	if gvk.Kind == "List" {
		return httpx.Forbidden("Kubernetes List manifests are not allowed through generic apply")
	}
	allowed := gvk.Group == "" && gvk.Version == "v1" && gvk.Kind == "Pod" ||
		gvk.Group == "apps" && gvk.Version == "v1" &&
			(gvk.Kind == "Deployment" || gvk.Kind == "StatefulSet" || gvk.Kind == "DaemonSet") ||
		gvk.Group == "batch" && gvk.Version == "v1" && (gvk.Kind == "Job" || gvk.Kind == "CronJob")
	if !allowed {
		return httpx.Forbidden("apiVersion/kind is not supported by generic workload apply; use the dedicated Kubernetes resource page")
	}
	return nil
}

// executeOn performs a validated change against a cluster.
func executeOn(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	if err := validateChange(&pl); err != nil {
		return nil, err
	}
	cs := kc.cs
	switch pl.Action {
	case "cordon", "uncordon":
		return nil, setUnschedulable(ctx, cs, pl.Name, pl.Action == "cordon")
	case "drain":
		var p drainParams
		_ = decodeParams(pl.Params, &p)
		return drainNode(ctx, cs, pl.Name, p)
	case "node_labels":
		var p labelsParams
		_ = decodeParams(pl.Params, &p)
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": p.Labels}})
		n, err := cs.CoreV1().Nodes().Patch(ctx, pl.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return gin_H{"name": n.Name, "labels": n.Labels}, nil
	case "node_taints":
		var p taintsParams
		_ = decodeParams(pl.Params, &p)
		n, err := cs.CoreV1().Nodes().Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		n.Spec.Taints = p.Taints
		n, err = cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return gin_H{"name": n.Name, "taints": n.Spec.Taints}, nil
	case "node_batch_labels":
		return executeNodeBatchLabels(ctx, kc, pl)
	case "node_batch_taints":
		return executeNodeBatchTaints(ctx, kc, pl)
	case "ns_create":
		var p nsParams
		_ = decodeParams(pl.Params, &p)
		ns, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Labels: p.Labels}},
			metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return namespaceRow(*ns), nil
	case "ns_delete":
		return gin_H{"name": pl.Namespace}, cs.CoreV1().Namespaces().Delete(ctx, pl.Namespace, metav1.DeleteOptions{})
	case "scale":
		var p scaleParams
		_ = decodeParams(pl.Params, &p)
		return scaleWorkload(ctx, cs, pl.Kind, pl.Namespace, pl.Name, *p.Replicas)
	case "restart":
		ts := time.Now().UTC().Format(time.RFC3339)
		patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, ts)
		_, err := kc.dyn.Resource(workloadGVR[pl.Kind]).Namespace(pl.Namespace).Patch(ctx, pl.Name, types.MergePatchType, []byte(patch),
			metav1.PatchOptions{FieldManager: fieldManager})
		return gin_H{"restarted_at": ts}, err
	case "delete":
		var p deleteParams
		_ = decodeParams(pl.Params, &p)
		bg := metav1.DeletePropagationBackground
		err := kc.dyn.Resource(workloadGVR[pl.Kind]).Namespace(pl.Namespace).Delete(ctx, pl.Name,
			metav1.DeleteOptions{PropagationPolicy: &bg, GracePeriodSeconds: p.GracePeriodSeconds})
		return gin_H{"deleted": pl.Name}, err
	case "apply":
		var p applyParams
		_ = decodeParams(pl.Params, &p)
		return applyYAML(ctx, kc, p)
	case "network_policy_create":
		var p networkPolicyParams
		_ = decodeNetworkPolicyParams(pl.Params, &p)
		created, err := cs.NetworkingV1().NetworkPolicies(pl.Namespace).Create(ctx, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: pl.Name, Namespace: pl.Namespace, Labels: p.Labels},
			Spec:       *p.Spec,
		}, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return networkPolicyRow(*created), nil
	case "network_policy_update":
		var p networkPolicyParams
		_ = decodeNetworkPolicyParams(pl.Params, &p)
		policies := cs.NetworkingV1().NetworkPolicies(pl.Namespace)
		current, err := policies.Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("NetworkPolicy changed since it was loaded; refresh and retry")
		}
		current.Labels = p.Labels
		current.Spec = *p.Spec
		updated, err := policies.Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return networkPolicyRow(*updated), nil
	case "network_policy_delete":
		var p networkPolicyParams
		_ = decodeNetworkPolicyParams(pl.Params, &p)
		uid, resourceVersion := p.UID, p.ResourceVersion
		err := cs.NetworkingV1().NetworkPolicies(pl.Namespace).Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
		})
		if err != nil {
			return nil, err
		}
		return gin_H{"namespace": pl.Namespace, "name": pl.Name, "deleted": true}, nil
	case "sc_create", "sc_update", "sc_delete", "pvc_create", "pvc_update", "pvc_delete",
		"pvc_batch_delete", "pv_create", "pv_update", "pv_delete", "snapshot_create", "snapshot_delete":
		return executeStorage(ctx, kc, pl)
	case "service_create", "service_update", "service_delete",
		"ingress_create", "ingress_update", "ingress_delete", "ingress_annotations":
		return executeNetworkResource(ctx, kc, pl)
	case "configmap_create", "configmap_update", "configmap_delete", "configmap_batch_delete",
		"secret_create", "secret_update", "secret_delete", "secret_batch_delete":
		return executeConfigResource(ctx, kc, pl)
	case "hpa_create", "hpa_update", "hpa_delete",
		"cronjob_create", "cronjob_update", "cronjob_delete", "cronjob_run":
		return executeAutoscalingCron(ctx, kc, pl)
	}
	return nil, errors.New("unreachable")
}

type gin_H = map[string]any //nolint:revive // keep ops free of gin import

func setUnschedulable(ctx context.Context, cs kubernetes.Interface, node string, v bool) error {
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, v)
	_, err := cs.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager})
	return err
}

// EvictResult is the outcome for one pod during a drain.
type EvictResult struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Status    string `json:"status"` // evicted | skipped | failed
	Reason    string `json:"reason,omitempty"`
}

// drainNode cordons the node and evicts its pods through the Eviction API
// (respecting PodDisruptionBudgets). It does not wait for pods to terminate.
func drainNode(ctx context.Context, cs kubernetes.Interface, node string, p drainParams) (any, error) {
	if err := setUnschedulable(ctx, cs, node, true); err != nil {
		return nil, err
	}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String()})
	if err != nil {
		return nil, err
	}
	ignoreDS := p.IgnoreDaemonSets == nil || *p.IgnoreDaemonSets
	out := []EvictResult{}
	evicted, failed := 0, 0
	for _, pod := range pods.Items {
		r := EvictResult{Namespace: pod.Namespace, Name: pod.Name}
		ctrl := metav1.GetControllerOf(&pod)
		switch {
		case pod.DeletionTimestamp != nil:
			r.Status, r.Reason = "skipped", "terminating"
		case pod.Annotations[corev1.MirrorPodAnnotationKey] != "":
			r.Status, r.Reason = "skipped", "mirror (static) pod"
		case pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed:
			r.Status, r.Reason = "skipped", "completed"
		case ctrl != nil && ctrl.Kind == "DaemonSet":
			if ignoreDS {
				r.Status, r.Reason = "skipped", "daemonset pod"
			} else {
				r.Status, r.Reason = "failed", "daemonset-managed pod (set ignore_daemonsets)"
			}
		case ctrl == nil && !p.Force:
			r.Status, r.Reason = "failed", "pod has no controller (set force)"
		case hasEmptyDir(pod) && !p.DeleteEmptyDirData:
			r.Status, r.Reason = "failed", "pod uses emptyDir (set delete_emptydir_data)"
		default:
			ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
			if p.GracePeriodSeconds != nil {
				ev.DeleteOptions = &metav1.DeleteOptions{GracePeriodSeconds: p.GracePeriodSeconds}
			}
			if err := cs.CoreV1().Pods(pod.Namespace).EvictV1(ctx, ev); err != nil {
				r.Status, r.Reason = "failed", err.Error()
				if apierrors.IsTooManyRequests(err) {
					r.Reason = "blocked by PodDisruptionBudget: " + err.Error()
				}
			} else {
				r.Status = "evicted"
			}
		}
		switch r.Status {
		case "evicted":
			evicted++
		case "failed":
			failed++
		}
		out = append(out, r)
	}
	return gin_H{"node": node, "cordoned": true, "evicted": evicted, "failed": failed, "pods": out}, nil
}

func hasEmptyDir(p corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if v.EmptyDir != nil {
			return true
		}
	}
	return false
}

func scaleWorkload(ctx context.Context, cs kubernetes.Interface, kind, ns, name string, replicas int32) (any, error) {
	sc := &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: autoscalingv1.ScaleSpec{Replicas: replicas}}
	var (
		cur *autoscalingv1.Scale
		err error
	)
	if kind == "statefulsets" {
		if cur, err = cs.AppsV1().StatefulSets(ns).GetScale(ctx, name, metav1.GetOptions{}); err == nil {
			sc.ResourceVersion = cur.ResourceVersion
			sc, err = cs.AppsV1().StatefulSets(ns).UpdateScale(ctx, name, sc, metav1.UpdateOptions{FieldManager: fieldManager})
		}
	} else {
		if cur, err = cs.AppsV1().Deployments(ns).GetScale(ctx, name, metav1.GetOptions{}); err == nil {
			sc.ResourceVersion = cur.ResourceVersion
			sc, err = cs.AppsV1().Deployments(ns).UpdateScale(ctx, name, sc, metav1.UpdateOptions{FieldManager: fieldManager})
		}
	}
	if err != nil {
		return nil, err
	}
	return gin_H{"kind": kind, "namespace": ns, "name": name, "from": cur.Spec.Replicas, "replicas": sc.Spec.Replicas}, nil
}

// AppliedObject is the result for one document of an apply.
type AppliedObject struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status"` // applied | failed
	Error      string `json:"error,omitempty"`
}

// applyYAML server-side-applies every document in a multi-doc YAML/JSON.
func applyYAML(ctx context.Context, kc *kubeClients, p applyParams) (any, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(p.YAML)), 4096)
	force := p.Force == nil || *p.Force // default: like kubectl apply --server-side --force-conflicts
	opts := metav1.ApplyOptions{FieldManager: fieldManager, Force: force}
	if p.DryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	results := []AppliedObject{}
	failed := 0
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, httpx.BadRequest("yaml decode: " + err.Error())
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetKind() == "List" {
			return nil, httpx.BadRequest("kind List is not supported; use multi-document YAML")
		}
		gvk := obj.GroupVersionKind()
		r := AppliedObject{APIVersion: obj.GetAPIVersion(), Kind: gvk.Kind, Name: obj.GetName()}
		res, err := applyOne(ctx, kc, obj, p.Namespace, opts)
		if err != nil {
			if meta.IsNoMatchError(err) {
				kc.mapper.Reset()
			}
			r.Status, r.Error = "failed", err.Error()
			failed++
		} else {
			r.Status, r.Namespace = "applied", res.GetNamespace()
		}
		results = append(results, r)
	}
	if len(results) == 0 {
		return nil, httpx.BadRequest("no objects in yaml")
	}
	return gin_H{"dry_run": p.DryRun, "failed": failed, "objects": results}, nil
}

func applyOne(ctx context.Context, kc *kubeClients, obj *unstructured.Unstructured, defNS string, opts metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	gvk := obj.GroupVersionKind()
	if obj.GetName() == "" || gvk.Kind == "" {
		return nil, errors.New("apiVersion, kind and metadata.name are required")
	}
	if err := rejectSensitiveApplyKind(gvk); err != nil {
		return nil, err
	}
	mapping, err := kc.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	obj.SetManagedFields(nil)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if obj.GetNamespace() == "" {
			if defNS == "" {
				defNS = "default"
			}
			obj.SetNamespace(defNS)
		}
		return kc.dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace()).Apply(ctx, obj.GetName(), obj, opts)
	}
	return nil, httpx.Forbidden("cluster-scoped object " + gvk.Kind + " is not allowed through generic apply")
}
