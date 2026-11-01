package cluster

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	networkReadLimit     = 500
	networkEndpointLimit = 2000
	networkReadTimeout   = 6 * time.Second
)

type networkReadStatus struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated"`
}

// Upstream error bodies may contain credentials or proxy details.
func networkReadFailure(err error) networkReadStatus {
	state, message := "error", "cluster resource request failed"
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		state, message = "forbidden", "cluster credentials cannot read this resource"
	case apierrors.IsNotFound(err), apierrors.IsServiceUnavailable(err):
		state, message = "unavailable", "resource API is unavailable"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		state, message = "error", "cluster resource request timed out or was canceled"
	}
	return networkReadStatus{State: state, Error: message}
}

type networkEndpointPod struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
}

type networkEndpoint struct {
	Addresses   []string            `json:"addresses"`
	Ready       *bool               `json:"ready"`
	Serving     *bool               `json:"serving"`
	Terminating *bool               `json:"terminating"`
	Pod         *networkEndpointPod `json:"pod,omitempty"`
}

type networkEndpointPort struct {
	Name     *string `json:"name"`
	Protocol string  `json:"protocol"`
	Port     *int32  `json:"port"`
}

type networkEndpointSlice struct {
	Name        string                `json:"name"`
	Namespace   string                `json:"namespace"`
	UID         types.UID             `json:"uid"`
	Service     string                `json:"service"`
	ServiceUID  types.UID             `json:"service_uid,omitempty"`
	AddressType string                `json:"address_type"`
	Ports       []networkEndpointPort `json:"ports"`
	Endpoints   []networkEndpoint     `json:"endpoints"`
}

type networkEndpointResult struct {
	networkReadStatus
	ClusterID int64                  `json:"cluster_id"`
	Items     []networkEndpointSlice `json:"items"`
}

func validNetworkScope(namespace, service string) error {
	if namespace != "" && len(validation.IsDNS1123Label(namespace)) != 0 {
		return httpx.BadRequest("invalid namespace")
	}
	if service != "" && len(validation.IsDNS1035Label(service)) != 0 {
		return httpx.BadRequest("invalid service name")
	}
	return nil
}

func (m *Module) listNetworkEndpoints(c *gin.Context) {
	namespace, service := c.Query("namespace"), c.Query("service")
	if err := validNetworkScope(namespace, service); err != nil {
		httpx.Fail(c, err)
		return
	}
	cl, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	result := readNetworkEndpoints(c.Request.Context(), kc, namespace, service)
	result.ClusterID = cl.ID
	httpx.OK(c, result)
}

func readNetworkEndpoints(ctx context.Context, kc *kubeClients, namespace, service string) networkEndpointResult {
	out := networkEndpointResult{Items: []networkEndpointSlice{}}
	if kc == nil || kc.cs == nil {
		out.networkReadStatus = networkReadStatus{State: "unavailable", Error: "cluster client unavailable"}
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, networkReadTimeout)
	defer cancel()
	opts := metav1.ListOptions{Limit: networkReadLimit}
	if service != "" {
		opts.LabelSelector = labels.Set{discoveryv1.LabelServiceName: service}.AsSelector().String()
	}
	list, err := kc.cs.DiscoveryV1().EndpointSlices(namespace).List(ctx, opts)
	if err != nil {
		out.networkReadStatus = networkReadFailure(err)
		return out
	}
	out.networkReadStatus = networkReadStatus{Available: true, State: "available", Truncated: list.Continue != ""}
	remaining := networkEndpointLimit
	for i, slice := range list.Items {
		if i >= networkReadLimit || remaining == 0 {
			out.Truncated = true
			break
		}
		if namespace != "" && slice.Namespace != namespace || service != "" && slice.Labels[discoveryv1.LabelServiceName] != service {
			continue
		}
		row, truncated := endpointSliceRow(slice, remaining)
		out.Truncated = out.Truncated || truncated
		if row.Service == "" {
			continue
		}
		remaining -= len(row.Endpoints)
		out.Items = append(out.Items, row)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Namespace != out.Items[j].Namespace {
			return out.Items[i].Namespace < out.Items[j].Namespace
		}
		return out.Items[i].Name < out.Items[j].Name
	})
	return out
}

func endpointSliceRow(slice discoveryv1.EndpointSlice, limit int) (networkEndpointSlice, bool) {
	row := networkEndpointSlice{
		Name: slice.Name, Namespace: slice.Namespace, UID: slice.UID,
		Service: slice.Labels[discoveryv1.LabelServiceName], AddressType: string(slice.AddressType),
		Ports: []networkEndpointPort{}, Endpoints: []networkEndpoint{},
	}
	if row.Service == "" || len(validation.IsDNS1035Label(row.Service)) != 0 {
		row.Service = ""
		return row, false
	}
	for _, owner := range slice.OwnerReferences {
		if owner.Kind == "Service" && owner.APIVersion == "v1" && owner.Name == row.Service {
			row.ServiceUID = owner.UID
			break
		}
	}
	truncated := false
	for i, port := range slice.Ports {
		if i >= 64 {
			truncated = true
			break
		}
		protocol := "TCP"
		if port.Protocol != nil {
			protocol = string(*port.Protocol)
		}
		row.Ports = append(row.Ports, networkEndpointPort{Name: port.Name, Port: port.Port, Protocol: protocol})
	}
	for i, endpoint := range slice.Endpoints {
		if i >= limit {
			truncated = true
			break
		}
		addresses := append([]string{}, endpoint.Addresses...)
		if len(addresses) > 16 {
			addresses, truncated = addresses[:16], true
		}
		item := networkEndpoint{
			Addresses: addresses, Ready: endpoint.Conditions.Ready,
			Serving: endpoint.Conditions.Serving, Terminating: endpoint.Conditions.Terminating,
		}
		// A name alone is insufficient to associate a replacement Pod. A
		// foreign-namespace target must never become a local Pod edge.
		ref := endpoint.TargetRef
		if ref != nil && ref.Kind == "Pod" && (ref.APIVersion == "" || ref.APIVersion == "v1") &&
			ref.Name != "" && ref.UID != "" && (ref.Namespace == "" || ref.Namespace == slice.Namespace) {
			item.Pod = &networkEndpointPod{Namespace: slice.Namespace, Name: ref.Name, UID: ref.UID}
		}
		row.Endpoints = append(row.Endpoints, item)
	}
	return row, truncated
}
