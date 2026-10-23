package cluster

import (
	"sort"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// NetworkServiceRow is the service discovery data used by the topology view.
type NetworkServiceRow struct {
	Namespace   string               `json:"namespace"`
	Name        string               `json:"name"`
	Type        corev1.ServiceType   `json:"type"`
	ClusterIP   string               `json:"cluster_ip"`
	ExternalIPs []string             `json:"external_ips"`
	Ports       []corev1.ServicePort `json:"ports"`
	Selector    map[string]string    `json:"selector"`
	Labels      map[string]string    `json:"labels"`
	CreatedAt   metav1.Time          `json:"created_at"`
}

func networkServiceRow(s corev1.Service) NetworkServiceRow {
	return NetworkServiceRow{
		Namespace: s.Namespace, Name: s.Name, Type: s.Spec.Type, ClusterIP: s.Spec.ClusterIP,
		ExternalIPs: s.Spec.ExternalIPs, Ports: s.Spec.Ports, Selector: s.Spec.Selector,
		Labels: s.Labels, CreatedAt: s.CreationTimestamp,
	}
}

// NetworkPolicyRow keeps the Kubernetes policy spec while omitting managedFields.
type NetworkPolicyRow struct {
	Namespace   string                                  `json:"namespace"`
	Name        string                                  `json:"name"`
	Labels      map[string]string                       `json:"labels"`
	PodSelector metav1.LabelSelector                    `json:"pod_selector"`
	PolicyTypes []networkingv1.PolicyType               `json:"policy_types"`
	Ingress     []networkingv1.NetworkPolicyIngressRule `json:"ingress"`
	Egress      []networkingv1.NetworkPolicyEgressRule  `json:"egress"`
	CreatedAt   metav1.Time                             `json:"created_at"`
}

func networkPolicyRow(p networkingv1.NetworkPolicy) NetworkPolicyRow {
	types := append([]networkingv1.PolicyType(nil), p.Spec.PolicyTypes...)
	if len(types) == 0 {
		types = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		if len(p.Spec.Egress) > 0 {
			types = append(types, networkingv1.PolicyTypeEgress)
		}
	}
	return NetworkPolicyRow{
		Namespace: p.Namespace, Name: p.Name, Labels: p.Labels, PodSelector: p.Spec.PodSelector,
		PolicyTypes: types, Ingress: p.Spec.Ingress, Egress: p.Spec.Egress, CreatedAt: p.CreationTimestamp,
	}
}

func (m *Module) listNetworkServices(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	services, err := kc.cs.CoreV1().Services(c.Query("namespace")).List(c.Request.Context(), metav1.ListOptions{LabelSelector: c.Query("label_selector")})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]NetworkServiceRow, 0, len(services.Items))
	for _, s := range services.Items {
		items = append(items, networkServiceRow(s))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) listNetworkPolicies(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	policies, err := kc.cs.NetworkingV1().NetworkPolicies(c.Query("namespace")).List(c.Request.Context(), metav1.ListOptions{LabelSelector: c.Query("label_selector")})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]NetworkPolicyRow, 0, len(policies.Items))
	for _, p := range policies.Items {
		items = append(items, networkPolicyRow(p))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}
