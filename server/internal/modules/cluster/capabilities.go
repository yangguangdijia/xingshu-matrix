package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

const (
	capabilityListLimit   = 500
	capabilityMaxPages    = 10
	capabilityMaxItems    = capabilityListLimit * capabilityMaxPages
	capabilityListTimeout = 6 * time.Second
)

var (
	errCapabilityTruncated = errors.New("集群资源超过读取上限，结果不完整")
	errCapabilityContinue  = errors.New("集群资源分页游标重复，结果不完整")
)

var capabilityResources = map[string]schema.GroupVersionResource{
	"ingress_classes":          {Group: "networking.k8s.io", Version: "v1", Resource: "ingressclasses"},
	"storage_classes":          {Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"},
	"volume_snapshot_classes":  snapshotClassGVR,
	"services":                 {Version: "v1", Resource: "services"},
	"ingresses":                {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	"network_policies":         {Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
	"service_accounts":         {Version: "v1", Resource: "serviceaccounts"},
	"roles":                    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	"role_bindings":            {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	"cluster_roles":            {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"cluster_role_bindings":    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
	"velero_backups":           {Group: "velero.io", Version: "v1", Resource: "backups"},
	"velero_schedules":         {Group: "velero.io", Version: "v1", Resource: "schedules"},
	"velero_storage_locations": {Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"},
}

type CapabilityItem struct {
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	APIVersion string            `json:"api_version,omitempty"`
	Resource   string            `json:"resource,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Source     string            `json:"source,omitempty"`
	State      string            `json:"state,omitempty"`
	Readiness  string            `json:"readiness,omitempty"`
}

type ClusterCapability struct {
	Available bool             `json:"available"`
	State     string           `json:"state"`
	Error     string           `json:"error,omitempty"`
	Items     []CapabilityItem `json:"items"`
	Truncated bool             `json:"truncated,omitempty"`
}

func emptyCapability(state, message string) ClusterCapability {
	return ClusterCapability{State: state, Error: message, Items: []CapabilityItem{}}
}

func classifyCapabilityError(err error) (string, string) {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return "forbidden", "无权读取集群资源"
	case errors.Is(err, errCapabilityTruncated):
		return "partial", errCapabilityTruncated.Error()
	case errors.Is(err, errCapabilityContinue):
		return "error", errCapabilityContinue.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "error", "读取集群资源超时"
	case errors.Is(err, context.Canceled):
		return "error", "读取集群资源已取消"
	case apierrors.IsNotFound(err):
		return "missing", "集群未提供请求的 API 资源"
	default:
		return "error", "读取集群资源失败"
	}
}

func mergeCapabilityError(current, next error) error {
	if current == nil {
		return next
	}
	if next == nil {
		return current
	}
	priority := map[string]int{"missing": 1, "partial": 2, "error": 3, "forbidden": 4}
	currentState, _ := classifyCapabilityError(current)
	nextState, _ := classifyCapabilityError(next)
	if priority[nextState] > priority[currentState] {
		return next
	}
	return current
}

// Discovery's convenience methods lack a context; use its REST client so the
// same request deadline also bounds API discovery. Fake clients have no REST client.
func capabilityAPIResources(ctx context.Context, kc *kubeClients, gv string) (*metav1.APIResourceList, error) {
	client := kc.cs.Discovery().RESTClient()
	if client == nil {
		return kc.cs.Discovery().ServerResourcesForGroupVersion(gv)
	}
	path := "/apis/" + gv
	if !strings.Contains(gv, "/") {
		path = "/api/" + gv
	}
	out := &metav1.APIResourceList{}
	err := client.Get().AbsPath(path).Do(ctx).Into(out)
	return out, err
}

func capabilityAPIGroups(ctx context.Context, kc *kubeClients) (*metav1.APIGroupList, error) {
	client := kc.cs.Discovery().RESTClient()
	if client == nil {
		return kc.cs.Discovery().ServerGroups()
	}
	out := &metav1.APIGroupList{}
	err := client.Get().AbsPath("/apis").Do(ctx).Into(out)
	return out, err
}

func capabilityPages[T any](ctx context.Context, opts metav1.ListOptions, list func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, error) {
	ctx, cancel := context.WithTimeout(ctx, capabilityListTimeout)
	defer cancel()
	if opts.Limit <= 0 || opts.Limit > capabilityListLimit {
		opts.Limit = capabilityListLimit
	}
	items := []T{}
	seen := map[string]bool{opts.Continue: true}
	for pageNumber := 0; pageNumber < capabilityMaxPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return items, err
		}
		page, next, err := list(ctx, opts)
		if err != nil {
			return items, err
		}
		if err := ctx.Err(); err != nil {
			return items, err
		}
		remaining := capabilityMaxItems - len(items)
		if len(page) > remaining {
			return append(items, page[:remaining]...), errCapabilityTruncated
		}
		items = append(items, page...)
		if next == "" {
			return items, nil
		}
		if seen[next] {
			return items, errCapabilityContinue
		}
		if len(items) >= capabilityMaxItems {
			return items, errCapabilityTruncated
		}
		seen[next] = true
		opts.Continue = next
	}
	return items, errCapabilityTruncated
}

func capabilityGV(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" {
		return gvr.Version
	}
	return gvr.Group + "/" + gvr.Version
}

func hasAPIVerb(resource metav1.APIResource, verb string) bool {
	for _, candidate := range resource.Verbs {
		if candidate == verb || candidate == "*" {
			return true
		}
	}
	return false
}

func capabilityResource(ctx context.Context, kc *kubeClients, gvr schema.GroupVersionResource, namespace string) ClusterCapability {
	if kc == nil || kc.cs == nil || kc.dyn == nil {
		return emptyCapability("error", "集群客户端不可用")
	}
	listCtx, cancel := context.WithTimeout(ctx, capabilityListTimeout)
	defer cancel()
	apiList, err := capabilityAPIResources(listCtx, kc, capabilityGV(gvr))
	if err != nil {
		state, message := classifyCapabilityError(err)
		return emptyCapability(state, message)
	}
	listable, registered := false, false
	for _, resource := range apiList.APIResources {
		if resource.Name == gvr.Resource {
			registered, listable = true, hasAPIVerb(resource, "list")
			break
		}
	}
	if !listable {
		if registered {
			return emptyCapability("forbidden", "API 已注册但未提供 list 读取能力")
		}
		return emptyCapability("missing", "API 未提供可读取的 "+gvr.Resource+" 资源")
	}

	var resource dynamic.ResourceInterface = kc.dyn.Resource(gvr).Namespace(namespace)
	items, err := capabilityPages(listCtx, metav1.ListOptions{Limit: capabilityListLimit}, func(ctx context.Context, opts metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
		list, err := resource.List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return list.Items, list.GetContinue(), nil
	})
	out := ClusterCapability{Available: err == nil, State: "available", Items: make([]CapabilityItem, 0, len(items))}
	if err != nil {
		out.State, out.Error = classifyCapabilityError(err)
		out.Truncated = len(items) > 0 || errors.Is(err, errCapabilityTruncated) || errors.Is(err, errCapabilityContinue)
	}
	for _, item := range items {
		out.Items = append(out.Items, capabilityItem(item))
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Namespace == out.Items[j].Namespace {
			return out.Items[i].Name < out.Items[j].Name
		}
		return out.Items[i].Namespace < out.Items[j].Namespace
	})
	return out
}

func capabilityItem(item unstructured.Unstructured) CapabilityItem {
	return CapabilityItem{
		Name: item.GetName(), Namespace: item.GetNamespace(), Kind: item.GetKind(),
		APIVersion: item.GetAPIVersion(), Labels: item.GetLabels(),
	}
}

func metricsCapability(ctx context.Context, kc *kubeClients) ClusterCapability {
	if kc == nil || kc.cs == nil || kc.metrics == nil {
		return emptyCapability("error", "metrics.k8s.io client is unavailable")
	}
	listCtx, cancel := context.WithTimeout(ctx, capabilityListTimeout)
	defer cancel()
	apiList, err := capabilityAPIResources(listCtx, kc, "metrics.k8s.io/v1beta1")
	if err != nil {
		state, message := classifyCapabilityError(err)
		return emptyCapability(state, message)
	}
	hasNodes, hasPods := false, false
	for _, resource := range apiList.APIResources {
		switch resource.Name {
		case "nodes":
			hasNodes = true
		case "pods":
			hasPods = true
		}
	}
	if !hasNodes || !hasPods {
		return emptyCapability("missing", "metrics.k8s.io/v1beta1 未同时提供 nodes 与 pods")
	}
	items, err := capabilityPages(listCtx, metav1.ListOptions{Limit: capabilityListLimit}, func(ctx context.Context, opts metav1.ListOptions) ([]metricsv1beta1.NodeMetrics, string, error) {
		list, err := kc.metrics.MetricsV1beta1().NodeMetricses().List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return list.Items, list.Continue, nil
	})
	out := ClusterCapability{Available: err == nil, State: "available", Items: make([]CapabilityItem, 0, len(items))}
	if err != nil {
		out.State, out.Error = classifyCapabilityError(err)
		out.Truncated = len(items) > 0 || errors.Is(err, errCapabilityTruncated) || errors.Is(err, errCapabilityContinue)
	}
	for _, item := range items {
		out.Items = append(out.Items, CapabilityItem{Name: item.Name, Kind: "NodeMetrics", APIVersion: "metrics.k8s.io/v1beta1"})
	}
	return out
}

type componentProbe struct {
	groups  []string
	markers []string
}

var componentProbes = map[string]componentProbe{
	"argocd":       {groups: []string{"argoproj.io"}, markers: []string{"argocd", "argocd-server", "argocd-application-controller", "argocd-repo-server"}},
	"apisix":       {groups: []string{"apisix.apache.org"}, markers: []string{"apisix"}},
	"istio":        {groups: []string{"networking.istio.io", "security.istio.io", "telemetry.istio.io", "install.istio.io"}, markers: []string{"istio", "istiod", "istio-ingressgateway", "istio-egressgateway"}},
	"cert_manager": {groups: []string{"cert-manager.io", "acme.cert-manager.io"}, markers: []string{"cert-manager"}},
	"tekton":       {groups: []string{"tekton.dev", "triggers.tekton.dev"}, markers: []string{"tekton"}},
	"jenkins":      {groups: []string{"jenkins.io"}, markers: []string{"jenkins"}},
}

func componentProbeForCatalogKey(key string) componentProbe {
	switch key {
	case "ingress-nginx":
		return componentProbe{markers: []string{"ingress-nginx"}}
	case "istio-base":
		return componentProbe{groups: []string{"networking.istio.io", "security.istio.io", "telemetry.istio.io", "install.istio.io"}}
	case "istiod":
		return componentProbe{groups: []string{"networking.istio.io", "security.istio.io"}, markers: []string{"istiod"}}
	case "istio-gateway":
		return componentProbe{groups: []string{"networking.istio.io"}, markers: []string{"istio-gateway"}}
	case "cert-manager":
		return componentProbes["cert_manager"]
	case "argocd", "jenkins":
		return componentProbes[key]
	case "prometheus":
		return componentProbe{groups: []string{"monitoring.coreos.com"}, markers: []string{"prometheus-kube-prometheus-prometheus", "prometheus"}}
	case "alertmanager":
		return componentProbe{groups: []string{"monitoring.coreos.com"}, markers: []string{"alertmanager-kube-prometheus-alertmanager", "alertmanager"}}
	case "loki":
		return componentProbe{markers: []string{"loki"}}
	case "jaeger":
		return componentProbe{groups: []string{"jaegertracing.io"}, markers: []string{"jaeger"}}
	case "tempo":
		return componentProbe{markers: []string{"tempo"}}
	case "elasticsearch":
		return componentProbe{markers: []string{"elasticsearch"}}
	default:
		return componentProbe{}
	}
}

type workloadInventory struct {
	items     []CapabilityItem
	err       error
	truncated bool
}

func capabilityWorkload(meta metav1.ObjectMeta, kind, readiness string) CapabilityItem {
	if meta.DeletionTimestamp != nil {
		readiness = "not_ready"
	}
	apiVersion := "apps/v1"
	source := "workload"
	if kind == "Pod" {
		apiVersion = "v1"
	} else if kind == "DaemonSet" {
		source = "node-workload"
	}
	return CapabilityItem{Name: meta.Name, Namespace: meta.Namespace, Labels: meta.Labels, Kind: kind,
		APIVersion: apiVersion, Source: source, State: "installed", Readiness: readiness}
}

func capabilityReady(ready bool) string {
	if ready {
		return "ready"
	}
	return "not_ready"
}

func listComponentWorkloads(ctx context.Context, kc *kubeClients) workloadInventory {
	if kc == nil || kc.cs == nil {
		return workloadInventory{err: errors.New("集群客户端不可用")}
	}
	listCtx, cancel := context.WithTimeout(ctx, capabilityListTimeout)
	defer cancel()
	out := workloadInventory{items: []CapabilityItem{}}
	opts := metav1.ListOptions{Limit: capabilityListLimit}
	deployments, err := listDeploymentsPaged(listCtx, kc, opts)
	if err != nil {
		out.err = mergeCapabilityError(out.err, err)
		out.truncated = out.truncated || len(out.items) > 0 ||
			errors.Is(err, errCapabilityTruncated) || errors.Is(err, errCapabilityContinue)
	}
	if deployments != nil {
		for _, item := range deployments.Items {
			desired := int32(1)
			if item.Spec.Replicas != nil {
				desired = *item.Spec.Replicas
			}
			ready := desired > 0 && item.Status.ObservedGeneration >= item.Generation &&
				item.Status.UpdatedReplicas >= desired && item.Status.AvailableReplicas >= desired
			out.items = append(out.items, capabilityWorkload(item.ObjectMeta, "Deployment", capabilityReady(ready)))
		}
	}
	statefulSets, err := listStatefulSetsPaged(listCtx, kc, opts)
	if err != nil {
		out.err = mergeCapabilityError(out.err, err)
		out.truncated = out.truncated || len(out.items) > 0 ||
			errors.Is(err, errCapabilityTruncated) || errors.Is(err, errCapabilityContinue)
	}
	if statefulSets != nil {
		for _, item := range statefulSets.Items {
			desired := int32(1)
			if item.Spec.Replicas != nil {
				desired = *item.Spec.Replicas
			}
			ready := desired > 0 && item.Status.ObservedGeneration >= item.Generation &&
				item.Status.ReadyReplicas >= desired && item.Status.UpdatedReplicas >= desired
			out.items = append(out.items, capabilityWorkload(item.ObjectMeta, "StatefulSet", capabilityReady(ready)))
		}
	}
	pods, err := listPodsPaged(listCtx, kc, opts)
	if err != nil {
		out.err = mergeCapabilityError(out.err, err)
		out.truncated = out.truncated || len(out.items) > 0 ||
			errors.Is(err, errCapabilityTruncated) || errors.Is(err, errCapabilityContinue)
	}
	if pods != nil {
		for _, item := range pods.Items {
			ready := false
			for _, condition := range item.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue && item.Status.Phase == corev1.PodRunning {
					ready = true
				}
			}
			out.items = append(out.items, capabilityWorkload(item.ObjectMeta, "Pod", capabilityReady(ready && item.DeletionTimestamp == nil)))
		}
	}
	daemonSets, err := listDaemonSetsPaged(listCtx, kc, opts)
	if err != nil {
		out.err = mergeCapabilityError(out.err, err)
		out.truncated = out.truncated || len(out.items) > 0 ||
			errors.Is(err, errCapabilityTruncated) || errors.Is(err, errCapabilityContinue)
	}
	if daemonSets != nil {
		for _, item := range daemonSets.Items {
			ready := item.Status.DesiredNumberScheduled > 0 && item.Status.ObservedGeneration >= item.Generation &&
				item.Status.NumberReady >= item.Status.DesiredNumberScheduled &&
				item.Status.UpdatedNumberScheduled >= item.Status.DesiredNumberScheduled
			out.items = append(out.items, capabilityWorkload(item.ObjectMeta, "DaemonSet", capabilityReady(ready)))
		}
	}
	return out
}

func listDeploymentsPaged(ctx context.Context, kc *kubeClients, opts metav1.ListOptions) (*appsv1.DeploymentList, error) {
	if kc == nil || kc.cs == nil {
		return nil, errors.New("集群客户端不可用")
	}
	items, err := capabilityPages(ctx, opts, func(ctx context.Context, opts metav1.ListOptions) ([]appsv1.Deployment, string, error) {
		page, err := kc.cs.AppsV1().Deployments("").List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	return &appsv1.DeploymentList{Items: items}, err
}
func listStatefulSetsPaged(ctx context.Context, kc *kubeClients, opts metav1.ListOptions) (*appsv1.StatefulSetList, error) {
	if kc == nil || kc.cs == nil {
		return nil, errors.New("集群客户端不可用")
	}
	items, err := capabilityPages(ctx, opts, func(ctx context.Context, opts metav1.ListOptions) ([]appsv1.StatefulSet, string, error) {
		page, err := kc.cs.AppsV1().StatefulSets("").List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	return &appsv1.StatefulSetList{Items: items}, err
}
func listDaemonSetsPaged(ctx context.Context, kc *kubeClients, opts metav1.ListOptions) (*appsv1.DaemonSetList, error) {
	if kc == nil || kc.cs == nil {
		return nil, errors.New("集群客户端不可用")
	}
	items, err := capabilityPages(ctx, opts, func(ctx context.Context, opts metav1.ListOptions) ([]appsv1.DaemonSet, string, error) {
		page, err := kc.cs.AppsV1().DaemonSets("").List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	return &appsv1.DaemonSetList{Items: items}, err
}
func listPodsPaged(ctx context.Context, kc *kubeClients, opts metav1.ListOptions) (*corev1.PodList, error) {
	if kc == nil || kc.cs == nil {
		return nil, errors.New("集群客户端不可用")
	}
	items, err := capabilityPages(ctx, opts, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.Pod, string, error) {
		page, err := kc.cs.CoreV1().Pods("").List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	return &corev1.PodList{Items: items}, err
}

func componentCapability(kc *kubeClients, key string, probe componentProbe, workloads workloadInventory) ClusterCapability {
	return componentCapabilityContext(context.Background(), kc, key, probe, workloads)
}

func componentWorkloadMatches(item CapabilityItem, probe componentProbe) bool {
	for _, label := range []string{"app.kubernetes.io/name", "app", "k8s-app"} {
		for _, marker := range probe.markers {
			if item.Labels[label] == marker {
				return true
			}
		}
	}
	return false
}

func componentCapabilityContext(ctx context.Context, kc *kubeClients, key string, probe componentProbe, workloads workloadInventory) ClusterCapability {
	if kc == nil || kc.cs == nil {
		return emptyCapability("error", "集群客户端不可用")
	}
	ctx, cancel := context.WithTimeout(ctx, capabilityListTimeout)
	defer cancel()
	groups, err := capabilityAPIGroups(ctx, kc)
	wanted := make(map[string]bool, len(probe.groups))
	for _, group := range probe.groups {
		wanted[group] = true
	}
	items := make([]CapabilityItem, 0)
	discoveryErr := err
	truncated := false
	foundGroup := false
	if groups != nil {
		for _, group := range groups.Groups {
			if !wanted[group.Name] {
				continue
			}
			foundGroup = true
			for i, version := range group.Versions {
				if i >= 8 {
					discoveryErr, truncated = mergeCapabilityError(discoveryErr, errCapabilityTruncated), true
					break
				}
				resources, err := capabilityAPIResources(ctx, kc, version.GroupVersion)
				if err != nil {
					discoveryErr = mergeCapabilityError(discoveryErr, err)
					truncated = truncated || len(items) > 0
					continue
				}
				for _, resource := range resources.APIResources {
					if strings.Contains(resource.Name, "/") {
						continue
					}
					if len(items) >= capabilityMaxItems {
						discoveryErr, truncated = mergeCapabilityError(discoveryErr, errCapabilityTruncated), true
						break
					}
					items = append(items, CapabilityItem{
						Name: resource.Name, Kind: resource.Kind, APIVersion: version.GroupVersion, Resource: resource.Name,
						Source: "api", State: "registered", Readiness: "unknown",
					})
				}
			}
		}
	}
	matched, ready := 0, 0
	for _, item := range workloads.items {
		if !componentWorkloadMatches(item, probe) {
			continue
		}
		if len(items) >= capabilityMaxItems {
			discoveryErr, truncated = mergeCapabilityError(discoveryErr, errCapabilityTruncated), true
			break
		}
		items = append(items, item)
		matched++
		if item.Readiness == "ready" {
			ready++
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			if items[i].Kind == items[j].Kind {
				return items[i].Name < items[j].Name
			}
			return items[i].Kind < items[j].Kind
		}
		return items[i].Namespace < items[j].Namespace
	})
	out := ClusterCapability{Items: items, Truncated: truncated}
	discoveryErr = mergeCapabilityError(discoveryErr, workloads.err)
	out.Truncated = out.Truncated || workloads.truncated
	if discoveryErr != nil {
		out.State, out.Error = classifyCapabilityError(discoveryErr)
		out.Truncated = out.Truncated || len(out.Items) > 0
		return out
	}
	if matched > 0 {
		if ready == matched {
			out.Available, out.State = true, "available"
		} else {
			out.State, out.Error = "partial", "已发现组件工作负载，但尚未全部就绪"
		}
		return out
	}
	if foundGroup {
		out.State, out.Error = "partial", "仅发现组件 API 注册，未发现匹配的控制器工作负载"
		return out
	}
	return emptyCapability("missing", fmt.Sprintf("未发现 %s API 或匹配的工作负载", key))
}

func veleroCapability(resources map[string]ClusterCapability) ClusterCapability {
	keys := []string{"velero_backups", "velero_schedules", "velero_storage_locations"}
	out := ClusterCapability{Items: []CapabilityItem{}}
	missing := make([]string, 0)
	for _, key := range keys {
		item := resources[key]
		out.Items = append(out.Items, CapabilityItem{Name: key, Kind: item.State, Resource: item.Error,
			State: item.State, Source: "api", Readiness: "unknown"})
		out.Truncated = out.Truncated || item.Truncated
		if !item.Available {
			if item.State == "partial" && out.State == "" {
				out.State, out.Error = "partial", "Velero API 读取结果不完整"
			}
			if item.State == "forbidden" || item.State == "error" {
				if out.State == "" || item.State == "forbidden" {
					out.State = item.State
				}
			}
			if item.State == "missing" {
				missing = append(missing, key)
			}
		}
	}
	allAvailable := true
	for _, key := range keys {
		if !resources[key].Available {
			allAvailable = false
			break
		}
	}
	if allAvailable {
		out.Available, out.State = true, "available"
		return out
	}
	if out.State == "" {
		out.State = "missing"
	}
	if len(missing) > 0 {
		out.Error = "Velero API 未完整提供：" + strings.Join(missing, ", ")
		if out.State != "forbidden" && out.State != "error" {
			out.State = "partial"
		}
	}
	return out
}

func (m *Module) clusterCapabilities(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	httpx.OK(c, gin.H{"capabilities": clusterCapabilities(c.Request.Context(), kc)})
}

func clusterCapabilities(ctx context.Context, kc *kubeClients) map[string]ClusterCapability {
	ctx, cancel := context.WithTimeout(ctx, 4*capabilityListTimeout)
	defer cancel()
	out := make(map[string]ClusterCapability, len(capabilityResources)+len(componentProbes)+1)
	for key, gvr := range capabilityResources {
		namespace := ""
		switch key {
		case "services", "ingresses", "network_policies", "service_accounts", "roles", "role_bindings",
			"velero_backups", "velero_schedules", "velero_storage_locations":
			namespace = metav1.NamespaceAll
		}
		out[key] = capabilityResource(ctx, kc, gvr, namespace)
	}
	out["metrics"] = metricsCapability(ctx, kc)
	workloads := listComponentWorkloads(ctx, kc)
	for key, probe := range componentProbes {
		out[key] = componentCapabilityContext(ctx, kc, key, probe, workloads)
	}
	out["velero"] = veleroCapability(out)
	return out
}

func veleroAPIsAvailable(ctx context.Context, kc *kubeClients) error {
	if kc == nil || kc.cs == nil {
		return httpx.NewError(503, httpx.CodeUnavailable, "集群客户端不可用")
	}
	ctx, cancel := context.WithTimeout(ctx, capabilityListTimeout)
	defer cancel()
	required := []struct {
		key  string
		verb string
	}{
		{key: "velero_backups", verb: "patch"},
		{key: "velero_schedules", verb: "patch"},
		{key: "velero_storage_locations", verb: "list"},
	}
	missing := make([]string, 0)
	for _, requirement := range required {
		gvr := capabilityResources[requirement.key]
		resources, err := capabilityAPIResources(ctx, kc, capabilityGV(gvr))
		if err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
				return httpx.NewError(403, httpx.CodeForbidden, "无权发现 Velero API")
			}
			if apierrors.IsNotFound(err) {
				missing = append(missing, gvr.Resource)
				continue
			}
			return httpx.NewError(502, httpx.CodeUpstream, "发现 Velero API 失败")
		}
		found := false
		for _, resource := range resources.APIResources {
			if resource.Name == gvr.Resource && hasAPIVerb(resource, requirement.verb) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, gvr.Resource+"("+requirement.verb+")")
		}
	}
	if len(missing) > 0 {
		return httpx.Conflict("Velero 备份组件不可用，缺少 API：" + strings.Join(missing, ", "))
	}
	return nil
}
