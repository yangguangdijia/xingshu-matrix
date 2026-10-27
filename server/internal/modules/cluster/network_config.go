package cluster

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

type networkResource struct {
	Group, Version, Resource, Kind string
	ClusterScoped                  bool
}

var ingressNetworkResources = []networkResource{
	{"networking.k8s.io", "v1", "ingresses", "Ingress", false},
	{"networking.k8s.io", "v1", "ingressclasses", "IngressClass", true},
}

var gatewayNetworkResources = []networkResource{
	{"gateway.networking.k8s.io", "v1", "gateways", "Gateway", false},
	{"gateway.networking.k8s.io", "v1", "httproutes", "HTTPRoute", false},
	{"gateway.networking.k8s.io", "v1", "grpcroutes", "GRPCRoute", false},
	{"gateway.networking.k8s.io", "v1", "gatewayclasses", "GatewayClass", true},
	{"apisix.apache.org", "v2", "apisixroutes", "ApisixRoute", false},
	{"networking.istio.io", "v1", "gateways", "Gateway", false},
	{"networking.istio.io", "v1", "virtualservices", "VirtualService", false},
}

var exposureNetworkResources = []networkResource{
	{"networking.k8s.io", "v1", "ingresses", "Ingress", false},
	{"apisix.apache.org", "v2", "apisixroutes", "ApisixRoute", false},
}

var networkResources = map[string][]networkResource{
	"ingress": ingressNetworkResources,
	"gateway": gatewayNetworkResources,
	"expose":  exposureNetworkResources,
	"mesh": {
		{"networking.istio.io", "v1", "virtualservices", "VirtualService", false},
		{"networking.istio.io", "v1", "destinationrules", "DestinationRule", false},
		{"security.istio.io", "v1", "peerauthentications", "PeerAuthentication", false},
		{"", "v1", "namespaces", "Namespace", true},
	},
	"policy": {
		{"networking.k8s.io", "v1", "networkpolicies", "NetworkPolicy", false},
		{"cilium.io", "v2", "ciliumnetworkpolicies", "CiliumNetworkPolicy", false},
		{"cilium.io", "v2", "ciliumclusterwidenetworkpolicies", "CiliumClusterwideNetworkPolicy", true},
	},
}

// Optional controllers are discovered by reading their actual resource APIs.
// Missing APIs, missing permissions and empty resource lists stay distinct.
func (m *Module) networkConfiguration(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	view := c.DefaultQuery("view", "ingress")
	resources, ok := networkResources[view]
	if !ok {
		httpx.Fail(c, httpx.Validation("view must be ingress, expose, gateway, mesh or policy"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	results := make([]gin.H, 0, len(resources))
	for _, resource := range resources {
		results = append(results, readNetworkResource(ctx, kc.dyn, resource, c.Query("namespace")))
	}
	httpx.OK(c, gin.H{"view": view, "resources": results})
}

func readNetworkResource(ctx context.Context, client dynamic.Interface, resource networkResource, namespace string) gin.H {
	apiVersion := resource.Version
	if resource.Group != "" {
		apiVersion = resource.Group + "/" + resource.Version
	}
	result := gin.H{"api_version": apiVersion, "kind": resource.Kind, "resource": resource.Resource,
		"cluster_scoped": resource.ClusterScoped, "available": false, "items": []any{}}
	gvr := schema.GroupVersionResource{Group: resource.Group, Version: resource.Version, Resource: resource.Resource}
	var ri dynamic.ResourceInterface = client.Resource(gvr)
	if !resource.ClusterScoped {
		ri = client.Resource(gvr).Namespace(namespace)
	}
	list, err := ri.List(ctx, metav1.ListOptions{Limit: 1000})
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			result["state"] = "not_installed"
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			result["state"] = "forbidden"
			result["error"] = "cluster credentials cannot read this API"
		default:
			result["state"] = "unavailable"
			result["error"] = "resource API request failed; check cluster connectivity and controller health"
		}
		return result
	}
	items := make([]map[string]any, 0, len(list.Items))
	for _, item := range list.Items {
		item.SetManagedFields(nil)
		// kubectl's last-applied annotation can duplicate credentials or
		// unrelated historical configuration; it is not needed for editing.
		annotations := item.GetAnnotations()
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		item.SetAnnotations(annotations)
		if item.GetKind() == "" {
			item.SetKind(resource.Kind)
			item.SetAPIVersion(apiVersion)
		}
		items = append(items, item.Object)
	}
	result["available"], result["state"], result["items"] = true, "available", items
	result["truncated"] = list.GetContinue() != ""
	return result
}
