package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return httpx.BadRequest("invalid params: " + err.Error())
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
	default:
		return httpx.BadRequest("unknown action " + pl.Action)
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
	obj.SetNamespace("")
	return kc.dyn.Resource(mapping.Resource).Apply(ctx, obj.GetName(), obj, opts)
}
