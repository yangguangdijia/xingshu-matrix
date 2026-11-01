package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func endpointTestPointer[T any](value T) *T { return &value }

func TestNetworkEndpointIdentityAndConditions(t *testing.T) {
	slice := discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps", UID: "slice-uid",
			Labels: map[string]string{discoveryv1.LabelServiceName: "web"},
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "Service", Name: "other", UID: "wrong-service"},
				{APIVersion: "v1", Kind: "Service", Name: "web", UID: "service-uid"},
			}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{
			{Name: endpointTestPointer("http"), Port: endpointTestPointer(int32(8080))},
		},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{
				Ready: endpointTestPointer(true), Serving: endpointTestPointer(true), Terminating: endpointTestPointer(false)},
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: "apps", Name: "web-1", UID: "pod-1"}},
			{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: endpointTestPointer(false)},
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: "other", Name: "web-1", UID: "foreign-pod"}},
			{Addresses: []string{"10.0.0.3"}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "web-2"}},
			{Addresses: []string{"10.0.0.4"}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "web-3", UID: "pod-3"}},
		},
	}
	row, truncated := endpointSliceRow(slice, 10)
	if truncated || row.ServiceUID != "service-uid" || row.Ports[0].Protocol != "TCP" {
		t.Fatalf("unexpected projection: %+v", row)
	}
	if row.Endpoints[0].Pod == nil || row.Endpoints[0].Pod.UID != "pod-1" || !*row.Endpoints[0].Ready ||
		!*row.Endpoints[0].Serving || *row.Endpoints[0].Terminating {
		t.Fatalf("Pod identity or conditions lost: %+v", row.Endpoints[0])
	}
	if row.Endpoints[1].Pod != nil || row.Endpoints[2].Pod != nil {
		t.Fatal("foreign namespace or missing UID created a Pod association")
	}
	if row.Endpoints[3].Pod == nil || row.Endpoints[3].Pod.Namespace != "apps" || row.Endpoints[3].Ready != nil {
		t.Fatal("implicit local namespace or unknown condition not retained")
	}
	row, truncated = endpointSliceRow(slice, 1)
	if !truncated || len(row.Endpoints) != 1 {
		t.Fatal("endpoint limit was not enforced")
	}
}

func TestNetworkEndpointReadFiltersAndLimits(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("list", "endpointslices", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listAction := action.(k8stesting.ListAction)
		if action.GetNamespace() != "apps" || listAction.GetListRestrictions().Labels.String() != "kubernetes.io/service-name=web" {
			t.Fatalf("unscoped request: %+v", action)
		}
		endpoints := make([]discoveryv1.Endpoint, networkEndpointLimit+1)
		return true, &discoveryv1.EndpointSliceList{Items: []discoveryv1.EndpointSlice{
			{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", Labels: map[string]string{
				discoveryv1.LabelServiceName: "web"}}, Endpoints: endpoints},
			{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "other", Labels: map[string]string{
				discoveryv1.LabelServiceName: "web"}}},
		}}, nil
	})
	out := readNetworkEndpoints(context.Background(), &kubeClients{cs: cs}, "apps", "web")
	if !out.Available || !out.Truncated || len(out.Items) != 1 || len(out.Items[0].Endpoints) != networkEndpointLimit {
		t.Fatalf("unexpected bounded result: %+v, slices=%d", out.networkReadStatus, len(out.Items))
	}
}

func TestNetworkEndpointFailuresAreDistinctAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		err         error
	}{
		{"forbidden", "forbidden", apierrors.NewForbidden(schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, "", errors.New("private-token"))},
		{"not-found", "unavailable", apierrors.NewNotFound(schema.GroupResource{Resource: "endpointslices"}, "")},
		{"transport", "error", errors.New("Authorization: private-token")},
		{"timeout", "error", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := kubefake.NewSimpleClientset()
			cs.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			out := readNetworkEndpoints(context.Background(), &kubeClients{cs: cs}, "", "")
			raw, err := json.Marshal(out)
			if err != nil || out.Available || out.State != tc.state || strings.Contains(string(raw), "private-token") {
				t.Fatalf("unsafe or incorrect state: %s %v", raw, err)
			}
		})
	}
	if result := readNetworkEndpoints(context.Background(), nil, "", ""); result.State != "unavailable" {
		t.Fatal("nil client should be unavailable")
	}
}

func TestNetworkReadRoutesRequireWorkloadPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	New().Register(router.Group("/api/v1"), &app.Deps{})
	for _, path := range []string{
		"/api/v1/clusters/1/network/endpoints",
		"/api/v1/clusters/1/network/configuration/detail",
		"/api/v1/clusters/1/integration-candidates",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusForbidden {
			t.Fatalf("unguarded route %s: %d %s", path, recorder.Code, recorder.Body)
		}
	}
}

func TestNetworkScopeValidation(t *testing.T) {
	for _, scope := range [][2]string{{"../other", "web"}, {"apps", "web,x=y"}, {"apps", "../web"}} {
		if validNetworkScope(scope[0], scope[1]) == nil {
			t.Fatalf("unsafe scope accepted: %v", scope)
		}
	}
	if err := validNetworkScope("apps", "web"); err != nil {
		t.Fatal(err)
	}
}
