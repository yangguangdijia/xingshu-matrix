package cluster

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
)

type networkConfigurationApplyParams struct {
	YAML      string `json:"yaml"`
	Namespace string `json:"namespace,omitempty"`
}

func validateNetworkConfigurationApply(pl *changePayload) error {
	var params networkConfigurationApplyParams
	if err := decodeParams(pl.Params, &params); err != nil {
		return err
	}
	obj, _, err := parseNetworkConfigurationManifest(params)
	if err != nil {
		return err
	}
	pl.Kind, pl.Name, pl.Namespace = obj.GetKind(), obj.GetName(), obj.GetNamespace()
	return nil
}

func parseNetworkConfigurationManifest(params networkConfigurationApplyParams) (*unstructured.Unstructured, schema.GroupVersionResource, error) {
	if strings.TrimSpace(params.YAML) == "" {
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("yaml is required")
	}
	if params.Namespace != "" && len(validation.IsDNS1123Label(params.Namespace)) > 0 {
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("invalid network configuration namespace")
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(params.YAML), 4096)
	var obj *unstructured.Unstructured
	for {
		document := make(map[string]any)
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, schema.GroupVersionResource{}, httpx.BadRequest("yaml decode: " + err.Error())
		}
		if len(document) == 0 {
			continue
		}
		if obj != nil {
			return nil, schema.GroupVersionResource{}, httpx.BadRequest("only one network resource is allowed")
		}
		obj = &unstructured.Unstructured{Object: document}
	}
	if obj == nil {
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("no object in yaml")
	}
	if obj.GetKind() == "List" {
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("kind List is not supported")
	}
	if obj.GetName() == "" || len(validation.IsDNS1123Subdomain(obj.GetName())) > 0 {
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("metadata.name must be a valid DNS subdomain")
	}

	gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
	if err != nil {
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("invalid apiVersion")
	}
	var gvr schema.GroupVersionResource
	switch {
	case gv.Group == "networking.k8s.io" && gv.Version == "v1" && obj.GetKind() == "Ingress":
		gvr = schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: "ingresses"}
	case gv.Group == "apisix.apache.org" && gv.Version != "" && obj.GetKind() == "ApisixRoute":
		gvr = schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: "apisixroutes"}
	default:
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("only networking.k8s.io/v1 Ingress and discovered ApisixRoute resources are allowed")
	}

	namespace := obj.GetNamespace()
	if namespace == "" {
		namespace = params.Namespace
	}
	if namespace == "" {
		namespace = "default"
	}
	if len(validation.IsDNS1123Label(namespace)) > 0 {
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("invalid network configuration namespace")
	}
	obj.SetNamespace(namespace)
	obj.SetManagedFields(nil)
	return obj, gvr, nil
}

func prepareNetworkConfigurationApply(ctx context.Context, kc *kubeClients, params networkConfigurationApplyParams) (*unstructured.Unstructured, schema.GroupVersionResource, error) {
	if kc == nil || kc.cs == nil || kc.dyn == nil {
		return nil, schema.GroupVersionResource{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "cluster client unavailable")
	}
	obj, gvr, err := parseNetworkConfigurationManifest(params)
	if err != nil {
		return nil, schema.GroupVersionResource{}, err
	}
	groupVersion := gvr.Group + "/" + gvr.Version
	resources, err := kc.cs.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		switch {
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			return nil, schema.GroupVersionResource{}, httpx.Forbidden("cluster credentials cannot discover " + obj.GetKind() + " API")
		case apierrors.IsNotFound(err):
			return nil, schema.GroupVersionResource{}, httpx.Conflict(obj.GetKind() + " API " + groupVersion + " is unavailable; install or enable the API before applying")
		default:
			return nil, schema.GroupVersionResource{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, obj.GetKind()+" API discovery failed")
		}
	}
	found := false
	if resources != nil {
		for _, resource := range resources.APIResources {
			if resource.Name == gvr.Resource && resource.Kind == obj.GetKind() && resource.Namespaced {
				found = true
				break
			}
		}
	}
	if !found {
		return nil, schema.GroupVersionResource{}, httpx.Conflict(obj.GetKind() + " API " + groupVersion + " is unavailable; install or enable the API before applying")
	}

	_, err = kc.cs.CoreV1().Namespaces().Get(ctx, obj.GetNamespace(), metav1.GetOptions{})
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		return nil, schema.GroupVersionResource{}, httpx.BadRequest("namespace " + obj.GetNamespace() + " does not exist")
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return nil, schema.GroupVersionResource{}, httpx.Forbidden("cluster credentials cannot verify namespace " + obj.GetNamespace())
	default:
		return nil, schema.GroupVersionResource{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "namespace validation failed")
	}
	return obj, gvr, nil
}

func applyNetworkConfiguration(ctx context.Context, kc *kubeClients, params networkConfigurationApplyParams) (any, error) {
	obj, gvr, err := prepareNetworkConfigurationApply(ctx, kc, params)
	if err != nil {
		return nil, err
	}
	applied, err := kc.dyn.Resource(gvr).Namespace(obj.GetNamespace()).Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: fieldManager,
		Force:        false,
	})
	if err != nil {
		return nil, kubeErr(err)
	}
	return gin.H{
		"api_version": applied.GetAPIVersion(),
		"kind":        applied.GetKind(),
		"name":        applied.GetName(),
		"namespace":   applied.GetNamespace(),
		"status":      "applied",
	}, nil
}

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

type networkConfigurationIdentity struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

type networkConfigurationDetailResult struct {
	networkReadStatus
	ClusterID int64          `json:"cluster_id"`
	Object    map[string]any `json:"object"`
}

func resolveNetworkConfigurationIdentity(identity networkConfigurationIdentity) (networkResource, error) {
	gv, err := schema.ParseGroupVersion(identity.APIVersion)
	if err != nil || gv.Version == "" {
		return networkResource{}, httpx.BadRequest("invalid api_version")
	}
	if identity.Name == "" || len(validation.IsDNS1123Subdomain(identity.Name)) != 0 {
		return networkResource{}, httpx.BadRequest("invalid resource name")
	}
	for _, catalog := range networkResources {
		for _, resource := range catalog {
			if resource.Group != gv.Group || resource.Kind != identity.Kind {
				continue
			}
			// Alternate served controller versions are allowed only for the
			// same allowlisted group/kind/resource, then checked by discovery.
			if len(validation.IsDNS1035Label(gv.Version)) != 0 {
				return networkResource{}, httpx.BadRequest("invalid API version")
			}
			resource.Version = gv.Version
			if resource.ClusterScoped {
				if identity.Namespace != "" {
					return networkResource{}, httpx.BadRequest("namespace is not allowed for a cluster-scoped resource")
				}
			} else if identity.Namespace == "" || len(validation.IsDNS1123Label(identity.Namespace)) != 0 {
				return networkResource{}, httpx.BadRequest("a valid namespace is required")
			}
			return resource, nil
		}
	}
	return networkResource{}, httpx.BadRequest("network resource group and kind are not allowed")
}

func (m *Module) networkConfigurationDetail(c *gin.Context) {
	identity := networkConfigurationIdentity{
		APIVersion: c.Query("api_version"), Kind: c.Query("kind"),
		Namespace: c.Query("namespace"), Name: c.Query("name"),
	}
	resource, err := resolveNetworkConfigurationIdentity(identity)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	cl, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	result := readNetworkConfigurationDetail(c.Request.Context(), kc, resource, identity)
	result.ClusterID = cl.ID
	httpx.OK(c, result)
}

func readNetworkConfigurationDetail(ctx context.Context, kc *kubeClients, resource networkResource, identity networkConfigurationIdentity) networkConfigurationDetailResult {
	out := networkConfigurationDetailResult{}
	if kc == nil || kc.cs == nil || kc.dyn == nil {
		out.networkReadStatus = networkReadStatus{State: "unavailable", Error: "cluster client unavailable"}
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, networkReadTimeout)
	defer cancel()
	// Use the context-bound discovery endpoint: cached discovery's
	// ServerResourcesForGroupVersion does not accept a request context.
	discoveryClient := kc.cs.Discovery()
	var apiList *metav1.APIResourceList
	var err error
	if restClient := discoveryClient.RESTClient(); restClient != nil {
		path := "/api/" + resource.Version
		if resource.Group != "" {
			path = "/apis/" + resource.Group + "/" + resource.Version
		}
		apiList = &metav1.APIResourceList{}
		err = restClient.Get().AbsPath(path).Do(ctx).Into(apiList)
	} else {
		apiList, err = discoveryClient.ServerResourcesForGroupVersion(identity.APIVersion)
	}
	if err != nil {
		out.networkReadStatus = networkReadFailure(err)
		return out
	}
	found := false
	if apiList != nil {
		for _, discovered := range apiList.APIResources {
			if discovered.Name == resource.Resource && discovered.Kind == resource.Kind &&
				discovered.Namespaced != resource.ClusterScoped && hasAPIVerb(discovered, "get") {
				found = true
				break
			}
		}
	}
	if !found {
		out.networkReadStatus = networkReadStatus{State: "unavailable", Error: "resource API does not advertise the requested get operation"}
		return out
	}
	gvr := schema.GroupVersionResource{Group: resource.Group, Version: resource.Version, Resource: resource.Resource}
	var ri dynamic.ResourceInterface = kc.dyn.Resource(gvr)
	if !resource.ClusterScoped {
		ri = kc.dyn.Resource(gvr).Namespace(identity.Namespace)
	}
	obj, err := ri.Get(ctx, identity.Name, metav1.GetOptions{})
	if err != nil {
		out.networkReadStatus = networkReadFailure(err)
		if apierrors.IsNotFound(err) {
			out.State, out.Error = "missing", "resource no longer exists"
		}
		return out
	}
	obj = obj.DeepCopy()
	obj.SetManagedFields(nil)
	annotations := obj.GetAnnotations()
	delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	obj.SetAnnotations(annotations)
	if obj.GetKind() == "" {
		obj.SetKind(resource.Kind)
	}
	if obj.GetAPIVersion() == "" {
		obj.SetAPIVersion(identity.APIVersion)
	}
	out.networkReadStatus = networkReadStatus{Available: true, State: "available"}
	out.Object = obj.Object
	return out
}
