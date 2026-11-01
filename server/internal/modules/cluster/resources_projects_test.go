package cluster

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeFake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestResourceProjectsUseRealNamespacesAndQuotaUsage(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	cs := kubeFake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps", UID: "ns-1", ResourceVersion: "7", Labels: map[string]string{"team": "web"}, CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}},
		&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "budget", Namespace: "apps"}, Spec: corev1.ResourceQuotaSpec{Hard: testProjectResources("requests.cpu", "2")}, Status: corev1.ResourceQuotaStatus{Hard: testProjectResources("requests.cpu", "2"), Used: testProjectResources("requests.cpu", "1500m")}},
		&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: "apps"}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer, Default: testProjectResources("cpu", "500m")}}}},
	)
	rows, sources, err := readProjectRows(context.Background(), &kubeClients{cs: cs}, "team=web", now)
	if err != nil || len(rows) != 1 || rows[0].Name != "apps" || rows[0].Phase != corev1.NamespaceActive || rows[0].Age != "60m" {
		t.Fatalf("namespace metadata/filter mismatch: %+v %v", rows, err)
	}
	if rows[0].ResourceQuotas[0].Used["requests.cpu"] != "1500m" || rows[0].LimitRanges[0].Limits[0].Default.Cpu().String() != "500m" ||
		sources["resource_quotas"].State != "available" || !sources["namespaces"].Complete {
		t.Fatalf("quota and limit values were not read from Kubernetes: %+v", rows)
	}
	rows, _, err = readProjectRows(context.Background(), &kubeClients{cs: cs}, "", now)
	if err != nil || len(rows) != 2 || rows[1].Age != "" || rows[1].ResourceQuotas == nil || rows[1].LimitRanges == nil {
		t.Fatal("empty configuration or unknown creation time became synthetic data")
	}
}

func TestResourceProjectsExposeUnavailableSourcesWithoutLosingNamespaces(t *testing.T) {
	cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}})
	cs.PrependReactor("list", "resourcequotas", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "resourcequotas"}, "", errors.New("private-marker"))
	})
	cs.PrependReactor("list", "limitranges", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("private-marker")
	})
	rows, sources, err := readProjectRows(context.Background(), &kubeClients{cs: cs}, "", time.Now())
	if err != nil || len(rows) != 1 || sources["resource_quotas"].State != "forbidden" || sources["limit_ranges"].State != "failed" ||
		rows[0].QuotaRead.Complete || rows[0].LimitRead.Complete {
		t.Fatalf("failed reads became an empty available configuration: %+v %+v %v", rows, sources, err)
	}
	if rows[0].QuotaRead.Error == "private-marker" {
		t.Fatal("upstream error content exposed")
	}
	cs.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", nil)
	})
	if _, _, err := readProjectRows(context.Background(), &kubeClients{cs: cs}, "", time.Now()); !apierrors.IsForbidden(err) {
		t.Fatal("namespace denial became a successful empty project inventory")
	}
}

func TestResourceProjectsMarkTruncatedInventories(t *testing.T) {
	cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}})
	cs.PrependReactor("list", "resourcequotas", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.ResourceQuotaList{ListMeta: metav1.ListMeta{Continue: "more"}, Items: make([]corev1.ResourceQuota, resourceListCap)}, nil
	})
	rows, sources, err := readProjectRows(context.Background(), &kubeClients{cs: cs}, "", time.Now())
	if err != nil || len(rows) != 1 || sources["resource_quotas"].Complete || !sources["resource_quotas"].Truncated || !rows[0].Truncated {
		t.Fatalf("incomplete quota inventory marked complete: %+v %+v %v", rows, sources, err)
	}
}

func TestResourceRoutesMountAndRequireAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	m := New()
	g := router.Group("/clusters")
	m.RegisterResourceRoutes(g)
	m.RegisterProjectQuotaRoutes(g)
	paths := []string{
		"/clusters/1/helm/releases", "/clusters/1/helm/releases/apps/web", "/clusters/1/helm/releases/apps/web/history",
		"/clusters/1/crds", "/clusters/1/crds/widgets.example.com",
		"/clusters/1/projects", "/clusters/1/projects/apps/quotas", "/clusters/1/projects/apps/limits",
	}
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != 401 {
			t.Fatalf("%s: expected authentication denial, got %d", path, recorder.Code)
		}
	}
}

func TestResourceWriteRoutesDenyAnonymousAndReadOnlyCallers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	routes := []struct{ method, path string }{
		{"POST", "/clusters/1/projects"},
		{"POST", "/clusters/1/quotas"},
		{"PATCH", "/clusters/1/quotas/apps/budget"},
		{"POST", "/clusters/1/limitranges"},
		{"PATCH", "/clusters/1/limitranges/apps/defaults"},
	}
	for _, principal := range []*rbac.Principal{nil, {Permissions: []string{PermNSRead}}} {
		router := gin.New()
		if principal != nil {
			router.Use(func(c *gin.Context) { rbac.SetPrincipal(c, principal) })
		}
		New().RegisterProjectQuotaRoutes(router.Group("/clusters"))
		for _, route := range routes {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(route.method, route.path, nil))
			want := 401
			if principal != nil {
				want = 403
			}
			if recorder.Code != want {
				t.Fatalf("%s %s: got %d, want %d", route.method, route.path, recorder.Code, want)
			}
		}
	}
}

func TestResourceProjectInputRejectsUnsafeNamesAndLabels(t *testing.T) {
	for _, input := range []nsParams{
		{Name: ""}, {Name: "MixedCase"}, {Name: "apps", Labels: map[string]string{"invalid key": "x"}},
		{Name: "apps", Labels: map[string]string{"team": "invalid value"}},
	} {
		if validateProjectInput(input) == nil {
			t.Fatalf("invalid namespace input accepted: %+v", input)
		}
	}
	if err := validateProjectInput(nsParams{Name: "apps", Labels: map[string]string{"example.com/team": "web"}}); err != nil {
		t.Fatalf("valid namespace rejected: %v", err)
	}
}

func TestResourceClusterAccessRequiresAuthenticatedExplicitEnvironment(t *testing.T) {
	for _, tt := range []struct {
		name   string
		p      *rbac.Principal
		cl     *Cluster
		status int
	}{
		{"anonymous", nil, &Cluster{Env: "test"}, 401},
		{"missing-cluster", &rbac.Principal{}, nil, 503},
		{"empty-env-unrestricted", &rbac.Principal{}, &Cluster{}, 403},
		{"blank-env-wildcard", &rbac.Principal{EnvScope: []string{"*"}}, &Cluster{Env: " "}, 403},
		{"explicit-denial", &rbac.Principal{EnvScope: []string{}}, &Cluster{Env: "test"}, 403},
		{"other-env", &rbac.Principal{EnvScope: []string{"dev"}}, &Cluster{Env: "test"}, 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := resourceClusterAccess(tt.p, tt.cl)
			var platform *httpx.Error
			if !errors.As(err, &platform) || platform.Status != tt.status {
				t.Fatalf("incorrect environment denial: %v", err)
			}
		})
	}
	for _, p := range []*rbac.Principal{{}, {EnvScope: []string{"test"}}, {EnvScope: []string{"*"}}} {
		if err := resourceClusterAccess(p, &Cluster{Env: "test"}); err != nil {
			t.Fatalf("authorized environment denied: %v", err)
		}
	}
}

func TestResourceProjectsPreserveAbsentQuotaSource(t *testing.T) {
	cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}})
	cs.PrependReactor("list", "resourcequotas", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "resourcequotas"}, "")
	})
	rows, sources, err := readProjectRows(context.Background(), &kubeClients{cs: cs}, "", time.Now())
	if err != nil || len(rows) != 1 || sources["resource_quotas"].State != "absent" ||
		rows[0].QuotaRead.Complete || rows[0].LimitRead.State != "available" {
		t.Fatalf("absent source collapsed into available empty configuration: %+v %+v %v", rows, sources, err)
	}
}
