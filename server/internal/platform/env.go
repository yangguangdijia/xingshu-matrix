// Package platform — 环境探测：给初始化向导提供真实的运行环境数据。
//
// 设计原则：能取到的用真实值，取不到的一律返回 null，由前端显示「未接入」——
// 绝不编造集群名、节点数、CPU/内存或镜像源。
package platform

import (
	"context"
	"os"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// EnvInfo is the environment probe result. Every optional field is nil when the
// value cannot be determined — the UI renders it as "未接入".
type EnvInfo struct {
	Deploy           string    `json:"deploy"` // kubernetes | binary
	Cluster          *string   `json:"cluster"`
	Namespace        *string   `json:"namespace"`
	Nodes            *int      `json:"nodes"`
	CPU              *Resource `json:"cpu"`
	Memory           *Resource `json:"memory"`
	EphemeralStorage *Resource `json:"ephemeral_storage"` // Node ephemeral storage; not PVC capacity.
	StorageClass     *string   `json:"storage_class"`
	StorageClasses   []string  `json:"storage_classes"`
	IngressClass     *string   `json:"ingress_class"`
	IngressOK        bool      `json:"ingress_available"`
	MetricsOK        bool      `json:"metrics_available"`
	Message          string    `json:"message,omitempty"`
}

// Resource carries allocatable (total) and requested (used) amounts.
type Resource struct {
	Used  float64 `json:"used"`
	Total float64 `json:"total"`
	Unit  string  `json:"unit"`
}

func registerEnv(r *gin.RouterGroup) {
	r.GET("/platform/env", rbac.RequirePerm("setting:read"), func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
		defer cancel()
		httpx.OK(c, probeEnv(ctx))
	})
}

func probeEnv(ctx context.Context) EnvInfo {
	info := EnvInfo{Deploy: "binary"}
	client, err := client()
	if err != nil {
		info.Message = "不在 Kubernetes 集群内运行：集群信息不可用（仅本进程配置）"
		return info
	}
	info.Deploy = "kubernetes"
	ns := namespace()
	info.Namespace = &ns
	if h := os.Getenv("KUBERNETES_SERVICE_HOST"); h != "" {
		info.Cluster = &h
	}

	if nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		n := len(nodes.Items)
		info.Nodes = &n
		if pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{}); err == nil {
			usage := requestedResources(pods.Items)
			allocatable := nodeAllocatableResources(nodes.Items)
			info.CPU = &Resource{Used: round2(usage.CPU), Total: round2(allocatable.CPU), Unit: "核"}
			info.Memory = &Resource{Used: round2(usage.Memory / gib), Total: round2(allocatable.Memory / gib), Unit: "GiB"}
			info.EphemeralStorage = &Resource{Used: round2(usage.Disk / gib), Total: round2(allocatable.Disk / gib), Unit: "GiB"}
		} else {
			info.Message = "无法读取 Pod 资源请求：CPU、内存和临时磁盘容量未接入"
		}
	}

	if classes, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{}); err == nil {
		info.StorageClasses, info.StorageClass = storageClassInfo(classes.Items)
	}
	// metrics.k8s.io 是否存在（决定 CPU/内存使用率能不能取到）
	if _, err := client.Discovery().ServerResourcesForGroupVersion("metrics.k8s.io/v1beta1"); err == nil {
		info.MetricsOK = true
	}
	if ics, err := client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{}); err == nil && len(ics.Items) > 0 {
		name := ics.Items[0].Name
		info.IngressClass = &name
		info.IngressOK = true
	}
	return info
}

const gib = 1024 * 1024 * 1024

type resourceUsage struct {
	CPU    float64
	Memory float64
	Disk   float64
}

func requestedResources(pods []corev1.Pod) resourceUsage {
	var cpu, mem, disk resource.Quantity
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			if v, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				cpu.Add(v)
			}
			if v, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				mem.Add(v)
			}
			if v, ok := c.Resources.Requests[corev1.ResourceEphemeralStorage]; ok {
				disk.Add(v)
			}
		}
	}
	return resourceUsage{
		CPU:    cpu.AsApproximateFloat64(),
		Memory: mem.AsApproximateFloat64(),
		Disk:   disk.AsApproximateFloat64(),
	}
}

func nodeAllocatableResources(nodes []corev1.Node) resourceUsage {
	var cpu, mem, disk resource.Quantity
	for _, node := range nodes {
		cpu.Add(node.Status.Allocatable[corev1.ResourceCPU])
		mem.Add(node.Status.Allocatable[corev1.ResourceMemory])
		disk.Add(node.Status.Allocatable[corev1.ResourceEphemeralStorage])
	}
	return resourceUsage{
		CPU:    cpu.AsApproximateFloat64(),
		Memory: mem.AsApproximateFloat64(),
		Disk:   disk.AsApproximateFloat64(),
	}
}

func storageClassInfo(classes []storagev1.StorageClass) ([]string, *string) {
	names := make([]string, 0, len(classes))
	defaults := make([]string, 0, 1)
	for _, sc := range classes {
		names = append(names, sc.Name)
		if isDefaultClass(sc) {
			defaults = append(defaults, sc.Name)
		}
	}
	sort.Strings(names)
	sort.Strings(defaults)
	var defaultName *string
	if len(defaults) == 1 {
		defaultName = &defaults[0]
	}
	return names, defaultName
}

func isDefaultClass(sc storagev1.StorageClass) bool {
	if sc.Annotations == nil {
		return false
	}
	return sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
		sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
