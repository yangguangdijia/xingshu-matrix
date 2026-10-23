package cicd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

type inventoryWorkload struct {
	Kind               string            `json:"kind"`
	Name               string            `json:"name"`
	Namespace          string            `json:"namespace"`
	Desired            int32             `json:"desired"`
	Ready              int32             `json:"ready"`
	Available          int32             `json:"available"`
	Images             []string          `json:"images"`
	Selector           map[string]string `json:"selector"`
	CreatedAt          time.Time         `json:"created_at"`
	ObservedGeneration int64             `json:"observed_generation"`
}

type inventoryContainer struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
}

type inventoryPod struct {
	Name       string               `json:"name"`
	Namespace  string               `json:"namespace"`
	Phase      string               `json:"phase"`
	Status     string               `json:"status"`
	Ready      string               `json:"ready"`
	Restarts   int32                `json:"restarts"`
	Node       string               `json:"node"`
	PodIP      string               `json:"pod_ip"`
	Owner      string               `json:"owner"`
	CreatedAt  time.Time            `json:"created_at"`
	Containers []inventoryContainer `json:"containers"`
}

type inventoryService struct {
	Name        string               `json:"name"`
	Namespace   string               `json:"namespace"`
	Type        corev1.ServiceType   `json:"type"`
	ClusterIP   string               `json:"cluster_ip"`
	ExternalIPs []string             `json:"external_ips"`
	Ports       []corev1.ServicePort `json:"ports"`
	Selector    map[string]string    `json:"selector"`
}

type applicationInventoryResult struct {
	Application string              `json:"application"`
	Environment string              `json:"environment"`
	TargetID    int64               `json:"target_id"`
	Namespace   string              `json:"namespace"`
	ObservedAt  time.Time           `json:"observed_at"`
	Workloads   []inventoryWorkload `json:"workloads"`
	Pods        []inventoryPod      `json:"pods"`
	Services    []inventoryService  `json:"services"`
	Warnings    []string            `json:"warnings"`
}

func (m *Module) applicationInventory(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	a, err := m.loadApp(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	env, err := a.env(c.Query("env"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if !rbac.PrincipalFrom(c).CanEnv(env.Name) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+env.Name))
		return
	}
	cs, _, _, err := m.targetClient(c, env.TargetID)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("application target: "+err.Error()))
		return
	}
	result, err := collectApplicationInventory(c.Request.Context(), cs, a.Name, *env)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	result.TargetID = env.TargetID
	httpx.OK(c, result)
}

func collectApplicationInventory(ctx context.Context, cs kubernetes.Interface, appName string, env EnvTarget) (*applicationInventoryResult, error) {
	namespace := strings.TrimSpace(env.Namespace)
	if namespace == "" {
		return nil, httpx.BadRequest("application environment namespace is empty")
	}
	out := &applicationInventoryResult{
		Application: appName, Environment: env.Name, Namespace: namespace, ObservedAt: time.Now().UTC(),
		Workloads: []inventoryWorkload{}, Pods: []inventoryPod{}, Services: []inventoryService{}, Warnings: []string{},
	}
	selector := labels.Set{}
	var deployments []appsv1.Deployment
	if env.Deployment != "" {
		d, err := cs.AppsV1().Deployments(namespace).Get(ctx, env.Deployment, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				out.Warnings = append(out.Warnings, fmt.Sprintf("Deployment %s/%s 不存在", namespace, env.Deployment))
				return out, nil
			}
			return nil, err
		}
		deployments = append(deployments, *d)
	} else if env.Helm != nil {
		release := strings.TrimSpace(env.Helm.Release)
		if release == "" {
			return nil, httpx.BadRequest("Helm release is empty")
		}
		selector["app.kubernetes.io/instance"] = release
	} else {
		return nil, httpx.BadRequest("application environment has no deployment or Helm release")
	}

	if len(deployments) > 0 {
		selector = labels.Set(deployments[0].Spec.Selector.MatchLabels)
		if len(deployments[0].Spec.Selector.MatchExpressions) > 0 {
			converted, err := metav1.LabelSelectorAsSelector(deployments[0].Spec.Selector)
			if err != nil {
				return nil, err
			}
			pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: converted.String()})
			if err != nil {
				return nil, err
			}
			out.Pods = mapInventoryPods(pods.Items)
			out.Workloads = mapInventoryWorkloads(deployments)
			services, err := cs.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			out.Services = mapInventoryServices(services.Items, deployments, pods.Items)
			return out, nil
		}
	} else {
		ds, err := cs.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.AsSelector().String()})
		if err != nil {
			return nil, err
		}
		deployments = ds.Items
	}
	if len(selector) == 0 {
		return nil, httpx.BadRequest("application workload has an empty pod selector")
	}
	selectorValue := selector.AsSelector().String()
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorValue})
	if err != nil {
		return nil, err
	}
	services, err := cs.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out.Workloads = mapInventoryWorkloads(deployments)
	out.Pods = mapInventoryPods(pods.Items)
	out.Services = mapInventoryServices(services.Items, deployments, pods.Items)
	return out, nil
}

func mapInventoryWorkloads(deployments []appsv1.Deployment) []inventoryWorkload {
	out := make([]inventoryWorkload, 0, len(deployments))
	for _, d := range deployments {
		w := inventoryWorkload{Kind: "Deployment", Name: d.Name, Namespace: d.Namespace, Ready: d.Status.ReadyReplicas,
			Available: d.Status.AvailableReplicas, Selector: d.Spec.Selector.MatchLabels, CreatedAt: d.CreationTimestamp.Time,
			ObservedGeneration: d.Status.ObservedGeneration}
		if d.Spec.Replicas != nil {
			w.Desired = *d.Spec.Replicas
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			w.Images = append(w.Images, c.Image)
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func mapInventoryPods(pods []corev1.Pod) []inventoryPod {
	out := make([]inventoryPod, 0, len(pods))
	for _, p := range pods {
		row := inventoryPod{Name: p.Name, Namespace: p.Namespace, Phase: string(p.Status.Phase), Status: string(p.Status.Phase),
			Node: p.Spec.NodeName, PodIP: p.Status.PodIP, CreatedAt: p.CreationTimestamp.Time}
		if p.Status.Reason != "" {
			row.Status = p.Status.Reason
		}
		ready := 0
		statuses := map[string]corev1.ContainerStatus{}
		for _, status := range p.Status.ContainerStatuses {
			statuses[status.Name] = status
			row.Restarts += status.RestartCount
			if status.Ready {
				ready++
			}
		}
		row.Ready = fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
		if owner := metav1.GetControllerOf(&p); owner != nil {
			row.Owner = owner.Kind + "/" + owner.Name
		}
		for _, spec := range p.Spec.Containers {
			status := statuses[spec.Name]
			container := inventoryContainer{Name: spec.Name, Image: spec.Image, Ready: status.Ready, Restarts: status.RestartCount}
			switch {
			case status.State.Waiting != nil:
				container.State, container.Reason = "waiting", status.State.Waiting.Reason
				if container.Reason != "" {
					row.Status = container.Reason
				}
			case status.State.Terminated != nil:
				container.State, container.Reason = "terminated", status.State.Terminated.Reason
			case status.State.Running != nil:
				container.State = "running"
			}
			row.Containers = append(row.Containers, container)
		}
		if p.DeletionTimestamp != nil {
			row.Status = "Terminating"
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func mapInventoryServices(services []corev1.Service, deployments []appsv1.Deployment, pods []corev1.Pod) []inventoryService {
	workloadLabels := make([]labels.Set, 0, len(deployments)+len(pods))
	for _, d := range deployments {
		workloadLabels = append(workloadLabels, labels.Set(d.Spec.Template.Labels))
	}
	for _, p := range pods {
		workloadLabels = append(workloadLabels, labels.Set(p.Labels))
	}
	out := make([]inventoryService, 0)
	for _, s := range services {
		if len(s.Spec.Selector) == 0 {
			continue
		}
		matched := false
		for _, candidate := range workloadLabels {
			if labels.Set(s.Spec.Selector).AsSelector().Matches(candidate) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		out = append(out, inventoryService{Name: s.Name, Namespace: s.Namespace, Type: s.Spec.Type, ClusterIP: s.Spec.ClusterIP,
			ExternalIPs: s.Spec.ExternalIPs, Ports: s.Spec.Ports, Selector: s.Spec.Selector})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
