package cluster

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	networkclient "k8s.io/client-go/kubernetes/typed/networking/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// Service and Ingress mutations use the same cluster.change gateway, env scope
// and audit path as every other Kubernetes write. Read models expose only the
// addresses Kubernetes actually reported in status; a LoadBalancer without a
// cloud controller therefore stays "pending" with no invented IP.

// IngressRow is an Ingress summary with its real controller status.
type IngressRow struct {
	Namespace     string                                    `json:"namespace"`
	Name          string                                    `json:"name"`
	UID           types.UID                                 `json:"uid"`
	ResourceVersion string                                  `json:"resource_version"`
	ClassName     string                                    `json:"class_name"`
	Hosts         []string                                  `json:"hosts"`
	Rules         []networkingv1.IngressRule                `json:"rules"`
	TLS           []networkingv1.IngressTLS                 `json:"tls"`
	LoadBalancer  []networkingv1.IngressLoadBalancerIngress `json:"load_balancer"`
	AddressStatus string                                    `json:"address_status"` // ready | pending
	Labels        map[string]string                         `json:"labels"`
	Annotations   map[string]string                         `json:"annotations"`
	CreatedAt     metav1.Time                               `json:"created_at"`
}

func ingressRow(in networkingv1.Ingress) IngressRow {
	hosts := []string{}
	for _, rule := range in.Spec.Rules {
		if rule.Host != "" {
			hosts = append(hosts, rule.Host)
		}
	}
	sort.Strings(hosts)
	status := "pending"
	if len(in.Status.LoadBalancer.Ingress) > 0 {
		status = "ready"
	}
	return IngressRow{Namespace: in.Namespace, Name: in.Name, UID: in.UID, ResourceVersion: in.ResourceVersion,
		ClassName: ingressClass(in), Hosts: hosts,
		Rules: in.Spec.Rules, TLS: in.Spec.TLS, LoadBalancer: in.Status.LoadBalancer.Ingress,
		AddressStatus: status, Labels: in.Labels, Annotations: in.Annotations, CreatedAt: in.CreationTimestamp}
}

func ingressClass(in networkingv1.Ingress) string {
	if in.Spec.IngressClassName != nil {
		return *in.Spec.IngressClassName
	}
	return in.Annotations["kubernetes.io/ingress.class"]
}

func (m *Module) listIngresses(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	list, err := kc.cs.NetworkingV1().Ingresses(c.Query("namespace")).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]IngressRow, 0, len(list.Items))
	for _, in := range list.Items {
		items = append(items, ingressRow(in))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) getIngress(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	in, err := kc.cs.NetworkingV1().Ingresses(c.Param("ns")).Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	httpx.OK(c, ingressRow(*in))
}

func (m *Module) getService(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	svc, err := kc.cs.CoreV1().Services(c.Param("ns")).Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	httpx.OK(c, networkServiceRow(*svc))
}

// ---------------------------------------------------------------- payloads

type serviceParams struct {
	Name               string               `json:"name"`
	Namespace          string               `json:"namespace"`
	Type               corev1.ServiceType   `json:"type"`
	ClusterIP          string               `json:"cluster_ip"`
	ExternalName       string               `json:"external_name"`
	Selector           map[string]string    `json:"selector"`
	Ports              []corev1.ServicePort `json:"ports"`
	ExternalIPs        []string             `json:"external_ips"`
	LoadBalancerIP     string               `json:"load_balancer_ip"`
	LoadBalancerClass  *string              `json:"load_balancer_class"`
	ExternalTrafficPol string               `json:"external_traffic_policy"`
	SessionAffinity    string               `json:"session_affinity"`
	Annotations        map[string]string    `json:"annotations"`
	Labels             map[string]string    `json:"labels"`
	UID                types.UID            `json:"uid"`
	ResourceVersion    string               `json:"resource_version"`
}

type ingressParams struct {
	Name            string                     `json:"name"`
	Namespace       string                     `json:"namespace"`
	ClassName       *string                    `json:"class_name"`
	Rules           []networkingv1.IngressRule `json:"rules"`
	TLS             []networkingv1.IngressTLS  `json:"tls"`
	Annotations     map[string]string          `json:"annotations"`
	Labels          map[string]string          `json:"labels"`
	UID             types.UID                  `json:"uid"`
	ResourceVersion string                     `json:"resource_version"`
}

func validServicePorts(ports []corev1.ServicePort) error {
	if len(ports) == 0 {
		return httpx.BadRequest("at least one service port is required")
	}
	seen := map[string]bool{}
	for _, p := range ports {
		if p.Port < 1 || p.Port > 65535 {
			return httpx.BadRequest("service port must be 1..65535")
		}
		if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP && p.Protocol != corev1.ProtocolUDP && p.Protocol != corev1.ProtocolSCTP {
			return httpx.BadRequest("service protocol must be TCP, UDP or SCTP")
		}
		if len(ports) > 1 {
			if errs := validation.IsDNS1123Label(p.Name); len(errs) > 0 {
				return httpx.BadRequest("each service port needs a unique DNS-label name when multiple ports are declared")
			}
		}
		key := p.Name + "/" + string(p.Protocol) + "/" + strconv.Itoa(int(p.Port))
		if seen[key] {
			return httpx.BadRequest("duplicate service port")
		}
		seen[key] = true
	}
	return nil
}

func validateServiceParams(p *serviceParams, creating bool) error {
	if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
		return httpx.BadRequest("invalid namespace")
	}
	if errs := validation.IsDNS1035Label(p.Name); len(errs) > 0 {
		return httpx.BadRequest("invalid Service name")
	}
	if p.Type == "" {
		p.Type = corev1.ServiceTypeClusterIP
	}
	switch p.Type {
	case corev1.ServiceTypeClusterIP, corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
		if err := validServicePorts(p.Ports); err != nil {
			return err
		}
	case corev1.ServiceTypeExternalName:
		if p.ExternalName == "" {
			return httpx.BadRequest("external_name is required for type ExternalName")
		}
	default:
		return httpx.BadRequest("type must be ClusterIP, NodePort, LoadBalancer or ExternalName")
	}
	if p.ExternalTrafficPol != "" && p.ExternalTrafficPol != string(corev1.ServiceExternalTrafficPolicyLocal) && p.ExternalTrafficPol != string(corev1.ServiceExternalTrafficPolicyCluster) {
		return httpx.BadRequest("external_traffic_policy must be Local or Cluster")
	}
	if p.SessionAffinity != "" && p.SessionAffinity != string(corev1.ServiceAffinityNone) && p.SessionAffinity != string(corev1.ServiceAffinityClientIP) {
		return httpx.BadRequest("session_affinity must be None or ClientIP")
	}
	if !creating && (p.UID == "" || p.ResourceVersion == "") {
		return httpx.BadRequest("uid and resource_version are required for Service update")
	}
	if creating && (p.UID != "" || p.ResourceVersion != "") {
		return httpx.BadRequest("create must not include uid or resource_version")
	}
	return nil
}

func validIngressHost(host string) bool {
	if host == "" {
		return true
	}
	if strings.HasPrefix(host, "*.") {
		host = host[2:]
	}
	if len(host) > 253 {
		return false
	}
	return len(validation.IsDNS1123Subdomain(host)) == 0
}

func validateIngressParams(p *ingressParams, creating bool) error {
	if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
		return httpx.BadRequest("invalid namespace")
	}
	if errs := validation.IsDNS1123Subdomain(p.Name); len(errs) > 0 {
		return httpx.BadRequest("invalid Ingress name")
	}
	if len(p.Rules) == 0 {
		return httpx.BadRequest("at least one ingress rule is required")
	}
	for _, rule := range p.Rules {
		if !validIngressHost(rule.Host) {
			return httpx.BadRequest("invalid Ingress host " + rule.Host)
		}
		if rule.HTTP == nil || len(rule.HTTP.Paths) == 0 {
			return httpx.BadRequest("each Ingress rule needs http.paths")
		}
		for _, path := range rule.HTTP.Paths {
			if path.Path == "" || !strings.HasPrefix(path.Path, "/") {
				return httpx.BadRequest("Ingress path must start with /")
			}
			if path.PathType == nil {
				return httpx.BadRequest("Ingress pathType is required")
			}
			switch *path.PathType {
			case networkingv1.PathTypePrefix, networkingv1.PathTypeExact, networkingv1.PathTypeImplementationSpecific:
			default:
				return httpx.BadRequest("invalid Ingress pathType")
			}
			if path.Backend.Service == nil || path.Backend.Service.Name == "" {
				return httpx.BadRequest("Ingress path backend service is required")
			}
			if errs := validation.IsDNS1035Label(path.Backend.Service.Name); len(errs) > 0 {
				return httpx.BadRequest("invalid backend Service name")
			}
		}
	}
	for _, tls := range p.TLS {
		if tls.SecretName != "" {
			if errs := validation.IsDNS1123Subdomain(tls.SecretName); len(errs) > 0 {
				return httpx.BadRequest("invalid TLS secret name")
			}
		}
		for _, host := range tls.Hosts {
			if !validIngressHost(host) {
				return httpx.BadRequest("invalid TLS host " + host)
			}
		}
	}
	if !creating && (p.UID == "" || p.ResourceVersion == "") {
		return httpx.BadRequest("uid and resource_version are required for Ingress update")
	}
	if creating && (p.UID != "" || p.ResourceVersion != "") {
		return httpx.BadRequest("create must not include uid or resource_version")
	}
	return nil
}

func validateNetworkResourceChange(pl *changePayload) error {
	switch pl.Action {
	case "service_create", "service_update":
		var p serviceParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if pl.Action == "service_update" {
			p.Name, p.Namespace = pl.Name, pl.Namespace
		}
		if p.Namespace == "" {
			p.Namespace = pl.Namespace
		}
		if p.Name == "" {
			p.Name = pl.Name
		}
		if err := validateServiceParams(&p, pl.Action == "service_create"); err != nil {
			return err
		}
		pl.Name, pl.Namespace = p.Name, p.Namespace
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "service_delete":
		if errs := validation.IsDNS1123Label(pl.Namespace); len(errs) > 0 {
			return httpx.BadRequest("invalid namespace")
		}
		if errs := validation.IsDNS1035Label(pl.Name); len(errs) > 0 {
			return httpx.BadRequest("invalid Service name")
		}
		if err := validateResourcePreconditions(pl.Params, "Service"); err != nil {
			return err
		}
	case "ingress_create", "ingress_update":
		var p ingressParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if pl.Action == "ingress_update" {
			p.Name, p.Namespace = pl.Name, pl.Namespace
		}
		if p.Namespace == "" {
			p.Namespace = pl.Namespace
		}
		if p.Name == "" {
			p.Name = pl.Name
		}
		if err := validateIngressParams(&p, pl.Action == "ingress_create"); err != nil {
			return err
		}
		pl.Name, pl.Namespace = p.Name, p.Namespace
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "ingress_delete", "ingress_annotations":
		if errs := validation.IsDNS1123Subdomain(pl.Name); len(errs) > 0 {
			return httpx.BadRequest("invalid Ingress name")
		}
		if errs := validation.IsDNS1123Label(pl.Namespace); len(errs) > 0 {
			return httpx.BadRequest("invalid namespace")
		}
		if pl.Action == "ingress_annotations" {
			var p struct {
				Annotations     map[string]string `json:"annotations"`
				UID             types.UID         `json:"uid"`
				ResourceVersion string            `json:"resource_version"`
			}
			if err := decodeParams(pl.Params, &p); err != nil {
				return err
			}
			if p.UID == "" || p.ResourceVersion == "" {
				return httpx.BadRequest("uid and resource_version are required")
			}
		} else if err := validateResourcePreconditions(pl.Params, "Ingress"); err != nil {
			return err
		}
	default:
		return httpx.BadRequest("unknown network action " + pl.Action)
	}
	return nil
}

// ---------------------------------------------------------------- execute

func executeNetworkResource(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	cs := kc.cs
	switch pl.Action {
	case "service_create", "service_update":
		var p serviceParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		spec := corev1.ServiceSpec{Type: p.Type, Selector: p.Selector, Ports: p.Ports, ExternalIPs: p.ExternalIPs,
			ExternalName: p.ExternalName, SessionAffinity: corev1.ServiceAffinity(p.SessionAffinity),
			LoadBalancerClass: p.LoadBalancerClass}
		if p.LoadBalancerIP != "" {
			spec.LoadBalancerIP = p.LoadBalancerIP
		}
		if p.ExternalTrafficPol != "" {
			spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicy(p.ExternalTrafficPol)
		}
		if pl.Action == "service_create" {
			if p.ClusterIP != "" {
				spec.ClusterIP = p.ClusterIP
			}
			created, err := cs.CoreV1().Services(p.Namespace).Create(ctx, &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels, Annotations: p.Annotations},
				Spec:       spec,
			}, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return networkServiceRow(*created), nil
		}
		current, err := cs.CoreV1().Services(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("Service changed since it was loaded; refresh and retry")
		}
		// ClusterIP(s) are immutable after creation; keep the allocated values
		// and other controller-assigned fields instead of treating them as input.
		spec.ClusterIP = current.Spec.ClusterIP
		spec.ClusterIPs = current.Spec.ClusterIPs
		spec.IPFamilies = current.Spec.IPFamilies
		spec.IPFamilyPolicy = current.Spec.IPFamilyPolicy
		spec.HealthCheckNodePort = current.Spec.HealthCheckNodePort
		spec.AllocateLoadBalancerNodePorts = current.Spec.AllocateLoadBalancerNodePorts
		spec.PublishNotReadyAddresses = current.Spec.PublishNotReadyAddresses
		spec.InternalTrafficPolicy = current.Spec.InternalTrafficPolicy
		if p.LoadBalancerClass == nil {
			spec.LoadBalancerClass = current.Spec.LoadBalancerClass
		}
		for i := range spec.Ports {
			for _, old := range current.Spec.Ports {
				if old.Name == spec.Ports[i].Name && old.Port == spec.Ports[i].Port {
					spec.Ports[i].NodePort = old.NodePort
					if spec.Ports[i].AppProtocol == nil {
						spec.Ports[i].AppProtocol = old.AppProtocol
					}
					break
				}
			}
		}
		current.Spec = spec
		if p.Labels != nil {
			current.Labels = p.Labels
		}
		if p.Annotations != nil {
			current.Annotations = p.Annotations
		}
		updated, err := cs.CoreV1().Services(p.Namespace).Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return networkServiceRow(*updated), nil
	case "service_delete":
		uid, rv, err := servicePreconditions(ctx, cs, pl.Namespace, pl.Name, pl.Params)
		if err != nil {
			return nil, err
		}
		if err := cs.CoreV1().Services(pl.Namespace).Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		}); err != nil {
			return nil, err
		}
		return gin.H{"namespace": pl.Namespace, "name": pl.Name, "deleted": true}, nil
	case "ingress_create", "ingress_update":
		var p ingressParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		if pl.Action == "ingress_create" {
			created, err := cs.NetworkingV1().Ingresses(p.Namespace).Create(ctx, &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels, Annotations: p.Annotations},
				Spec:       networkingv1.IngressSpec{IngressClassName: p.ClassName, Rules: p.Rules, TLS: p.TLS},
			}, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return ingressRow(*created), nil
		}
		current, err := cs.NetworkingV1().Ingresses(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("Ingress changed since it was loaded; refresh and retry")
		}
		current.Spec = networkingv1.IngressSpec{IngressClassName: p.ClassName, Rules: p.Rules, TLS: p.TLS}
		if p.Labels != nil {
			current.Labels = p.Labels
		}
		if p.Annotations != nil {
			current.Annotations = p.Annotations
		}
		updated, err := cs.NetworkingV1().Ingresses(p.Namespace).Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return ingressRow(*updated), nil
	case "ingress_delete":
		uid, rv, err := ingressPreconditions(ctx, cs, pl.Namespace, pl.Name, pl.Params)
		if err != nil {
			return nil, err
		}
		if err := cs.NetworkingV1().Ingresses(pl.Namespace).Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		}); err != nil {
			return nil, err
		}
		return gin.H{"namespace": pl.Namespace, "name": pl.Name, "deleted": true}, nil
	case "ingress_annotations":
		var p struct {
			Annotations     map[string]string `json:"annotations"`
			UID             types.UID         `json:"uid"`
			ResourceVersion string            `json:"resource_version"`
		}
		_ = decodeParams(pl.Params, &p)
		current, err := cs.NetworkingV1().Ingresses(pl.Namespace).Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("Ingress changed since it was loaded; refresh and retry")
		}
		current.Annotations = p.Annotations
		updated, err := cs.NetworkingV1().Ingresses(pl.Namespace).Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return ingressRow(*updated), nil
	}
	return nil, httpx.BadRequest("unknown network action " + pl.Action)
}

func servicePreconditions(ctx context.Context, cs interface {
	CoreV1() coreclient.CoreV1Interface
}, namespace, name string, raw json.RawMessage) (types.UID, string, error) {
	var p struct {
		UID             types.UID `json:"uid"`
		ResourceVersion string    `json:"resource_version"`
	}
	_ = decodeParams(raw, &p)
	if p.UID == "" || p.ResourceVersion == "" {
		return "", "", httpx.BadRequest("uid and resource_version are required")
	}
	svc, err := cs.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if svc.UID != p.UID || svc.ResourceVersion != p.ResourceVersion {
		return "", "", httpx.Conflict("Service changed since it was loaded; refresh and retry")
	}
	return p.UID, p.ResourceVersion, nil
}

func ingressPreconditions(ctx context.Context, cs interface {
	NetworkingV1() networkclient.NetworkingV1Interface
}, namespace, name string, raw json.RawMessage) (types.UID, string, error) {
	var p struct {
		UID             types.UID `json:"uid"`
		ResourceVersion string    `json:"resource_version"`
	}
	_ = decodeParams(raw, &p)
	if p.UID == "" || p.ResourceVersion == "" {
		return "", "", httpx.BadRequest("uid and resource_version are required")
	}
	in, err := cs.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if in.UID != p.UID || in.ResourceVersion != p.ResourceVersion {
		return "", "", httpx.Conflict("Ingress changed since it was loaded; refresh and retry")
	}
	return p.UID, p.ResourceVersion, nil
}
