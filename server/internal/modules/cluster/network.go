package cluster

import (
	"sort"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// NetworkServiceRow is the service discovery data used by the topology view.
type NetworkServiceRow struct {
	Namespace           string                         `json:"namespace"`
	Name                string                         `json:"name"`
	UID                 types.UID                      `json:"uid"`
	ResourceVersion     string                         `json:"resource_version"`
	Type                corev1.ServiceType             `json:"type"`
	ClusterIP           string                         `json:"cluster_ip"`
	ClusterIPs          []string                       `json:"cluster_ips"`
	ExternalIPs         []string                       `json:"external_ips"`
	ExternalName        string                         `json:"external_name,omitempty"`
	LoadBalancer        []corev1.LoadBalancerIngress  `json:"load_balancer"`
	LoadBalancerIP      string                         `json:"load_balancer_ip,omitempty"`
	AddressStatus       string                         `json:"address_status"`
	LoadBalancerClass   string                         `json:"load_balancer_class,omitempty"`
	ExternalTrafficPol  corev1.ServiceExternalTrafficPolicy `json:"external_traffic_policy,omitempty"`
	SessionAffinity     corev1.ServiceAffinity         `json:"session_affinity,omitempty"`
	Ports               []corev1.ServicePort           `json:"ports"`
	Selector            map[string]string              `json:"selector"`
	Labels              map[string]string              `json:"labels"`
	Annotations         map[string]string              `json:"annotations"`
	CreatedAt           metav1.Time                    `json:"created_at"`
}

func networkServiceRow(s corev1.Service) NetworkServiceRow {
	addressStatus := "not_applicable"
	if s.Spec.Type == corev1.ServiceTypeLoadBalancer {
		addressStatus = "pending"
		if len(s.Status.LoadBalancer.Ingress) > 0 {
			addressStatus = "ready"
		}
	}
	class := ""
	if s.Spec.LoadBalancerClass != nil {
		class = *s.Spec.LoadBalancerClass
	}
	return NetworkServiceRow{
		Namespace: s.Namespace, Name: s.Name, UID: s.UID, ResourceVersion: s.ResourceVersion,
		Type: s.Spec.Type, ClusterIP: s.Spec.ClusterIP,
		ClusterIPs: s.Spec.ClusterIPs, ExternalIPs: s.Spec.ExternalIPs, ExternalName: s.Spec.ExternalName,
		LoadBalancer: s.Status.LoadBalancer.Ingress, LoadBalancerIP: s.Spec.LoadBalancerIP,
		AddressStatus: addressStatus, LoadBalancerClass: class,
		ExternalTrafficPol: s.Spec.ExternalTrafficPolicy, SessionAffinity: s.Spec.SessionAffinity,
		Ports: s.Spec.Ports, Selector: s.Spec.Selector, Labels: s.Labels, Annotations: s.Annotations,
		CreatedAt: s.CreationTimestamp,
	}
}

// NetworkPolicyRow keeps the Kubernetes policy spec while omitting managedFields.
type NetworkPolicyRow struct {
	Namespace       string                                  `json:"namespace"`
	Name            string                                  `json:"name"`
	UID             types.UID                               `json:"uid"`
	ResourceVersion string                                  `json:"resource_version"`
	Labels          map[string]string                       `json:"labels"`
	PodSelector     metav1.LabelSelector                    `json:"pod_selector"`
	PolicyTypes     []networkingv1.PolicyType               `json:"policy_types"`
	Ingress         []networkingv1.NetworkPolicyIngressRule `json:"ingress"`
	Egress          []networkingv1.NetworkPolicyEgressRule  `json:"egress"`
	CreatedAt       metav1.Time                             `json:"created_at"`
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
		Namespace: p.Namespace, Name: p.Name, UID: p.UID, ResourceVersion: p.ResourceVersion,
		Labels: p.Labels, PodSelector: p.Spec.PodSelector,
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
