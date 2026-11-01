package cluster

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	helmfake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

func TestComponentCatalogIsPinnedAndDependenciesOrdered(t *testing.T) {
	want := []string{"alertmanager", "argocd", "cert-manager", "elasticsearch", "ingress-nginx", "istio-base", "istio-gateway", "istiod", "jaeger", "jenkins", "loki", "prometheus", "tempo", "traefik"}
	catalog := sortedComponentCatalog()
	if len(catalog) != len(want) {
		t.Fatalf("catalog has %d components, want %d", len(catalog), len(want))
	}
	for i, def := range catalog {
		if def.Key != want[i] {
			t.Fatalf("component %d = %s, want %s", i, def.Key, want[i])
		}
		if def.Key == "ingress-nginx" || def.Key == "elasticsearch" {
			if def.Supported || def.Installable || len(def.ConfigurationFields) != 0 || len(def.Versions) != 0 {
				t.Fatalf("discovery-only component exposes install: %s", def.Key)
			}
			continue
		}
		p, ok := componentProfileFor(def.Key)
		if !ok || !def.Supported || !def.Installable || len(def.Versions) != 1 || def.Versions[0] != p.Versions[0] || def.Chart != p.Chart || def.RepoURL != "https://"+p.RepoHost+p.RepoPath || def.KubeVersion == "" {
			t.Fatalf("component contract is incomplete: %s", def.Key)
		}
		for _, dependency := range def.Dependencies {
			if dependency == def.Key {
				t.Fatalf("self dependency: %s", def.Key)
			}
			if _, ok := componentDefinition(dependency); !ok {
				t.Fatalf("missing dependency: %s", dependency)
			}
		}
	}
	d, ok := componentDefinition("istiod")
	if !ok || len(d.Dependencies) != 1 || d.Dependencies[0] != "istio-base" {
		t.Fatal("istio dependency missing")
	}
	nginx, ok := componentDefinition("ingress-nginx")
	if !ok || nginx.Supported || nginx.Installable {
		t.Fatal("retired ingress-nginx chart must remain unavailable")
	}
}

func TestComponentInstallRequiresPermissionsForAllRenderedResourceKinds(t *testing.T) {
	for _, permissions := range [][]string{
		{componentWritePermission},
		{componentWritePermission, PermConfigWrite},
		{componentWritePermission, PermRBACWrite},
	} {
		if componentPrincipalCanInstall(&rbac.Principal{Permissions: permissions}) {
			t.Fatalf("incomplete install permissions accepted: %v", permissions)
		}
	}
	if !componentPrincipalCanInstall(&rbac.Principal{Permissions: []string{componentWritePermission, PermRBACWrite, PermConfigWrite}}) {
		t.Fatal("complete install permissions rejected")
	}
}

func TestComponentOperationFailureStatusDoesNotRetryAmbiguousMutation(t *testing.T) {
	if componentFailureStatus("failed") != "failed" || componentFailureStatus("unknown") != "unknown" ||
		componentFailureStatus("pending-install") != "unknown" {
		t.Fatal("ambiguous Helm outcome must prevent automatic retry")
	}
}

func TestComponentApprovalRedactionsBindSnapshotFieldSet(t *testing.T) {
	requested := map[string]string{"replicas": "2", "service.type": "NodePort"}
	if !componentApprovalRedactionsMatch(map[string]string{"replicas": "[redacted]", "service.type": "[redacted]"}, requested) {
		t.Fatal("matching redacted approval fields rejected")
	}
	for _, got := range []map[string]string{
		{"replicas": "[redacted]"},
		{"replicas": "[redacted]", "service.type": "[redacted]", "extra": "[redacted]"},
		{"replicas": "2", "service.type": "[redacted]"},
	} {
		if componentApprovalRedactionsMatch(got, requested) {
			t.Fatalf("mismatched approval fields accepted: %#v", got)
		}
	}
}

func TestComponentPinnedArtifactFetch(t *testing.T) {
	if os.Getenv("MATRIX_TEST_LIVE_COMPONENT_CHART") != "1" {
		t.Skip("set MATRIX_TEST_LIVE_COMPONENT_CHART=1 to fetch the pinned public chart")
	}
	def, ok := componentDefinition("traefik")
	if !ok {
		t.Fatal("pinned component is missing")
	}
	if _, digest, err := fetchVerifiedComponentChart(context.Background(), def, ""); err != nil || len(digest) != 64 {
		t.Fatalf("pinned artifact fetch failed: %v", err)
	}
}

func TestComponentHelmSDKInstallFixture(t *testing.T) {
	fixture := &chart.Chart{
		Metadata: &chart.Metadata{Name: "traefik", Version: "41.7.1"},
		Templates: []*chart.File{{Name: "templates/check.yaml", Data: []byte(
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-check\n  namespace: {{ .Release.Namespace }}\n",
		)}},
	}
	cfg := &action.Configuration{Releases: storage.Init(driver.NewMemory()),
		KubeClient: &helmfake.PrintingKubeClient{Out: io.Discard}, Capabilities: chartutil.DefaultCapabilities,
		Log: func(string, ...interface{}) {}}
	in := componentInstallRequest{Component: "traefik", Namespace: "ingress-system", ReleaseName: "edge", Version: "41.7.1"}
	status, err := runVerifiedComponentInstallWithConfig(context.Background(), cfg, fixture, map[string]any{}, in)
	if err != nil || status != "deployed" {
		t.Fatalf("SDK fixture result=%q err=%v", status, err)
	}
	stored, err := action.NewGet(cfg).Run(in.ReleaseName)
	if err != nil || stored.Info.Status.String() != status {
		t.Fatalf("SDK release persistence: %v", err)
	}
}

func TestComponentPreflightUsesClusterPermissionsAndServerDryRun(t *testing.T) {
	for _, scenario := range []string{"ready", "namespace-terminating", "release-conflict", "forbidden", "dry-run-rejected", "profile-kube-incompatible"} {
		t.Run(scenario, func(t *testing.T) {
			dryRuns := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respond := func(value any) { capabilityTestJSON(t, w, value) }
				switch r.URL.Path {
				case "/api/v1/namespaces/ingress-system":
					phase := corev1.NamespaceActive
					if scenario == "namespace-terminating" {
						phase = corev1.NamespaceTerminating
					}
					respond(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ingress-system"}, Status: corev1.NamespaceStatus{Phase: phase}})
				case "/api/v1/namespaces/ingress-system/secrets":
					items := []corev1.Secret{}
					if scenario == "release-conflict" {
						items = append(items, corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "existing-release"}})
					}
					respond(&corev1.SecretList{Items: items})
				case "/readyz":
					_, _ = w.Write([]byte("ok"))
				case "/version":
					version := "v1.32.2"
					if scenario == "profile-kube-incompatible" {
						version = "v1.24.0"
					}
					respond(map[string]string{"gitVersion": version})
				case "/api":
					respond(&metav1.APIVersions{Versions: []string{"v1"}})
				case "/apis":
					version := metav1.GroupVersionForDiscovery{GroupVersion: "networking.k8s.io/v1", Version: "v1"}
					respond(&metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "networking.k8s.io", Versions: []metav1.GroupVersionForDiscovery{version}, PreferredVersion: version}}})
				case "/api/v1":
					respond(&metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{
						{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: []string{"get", "list", "create", "watch"}},
					}})
				case "/apis/networking.k8s.io/v1":
					respond(&metav1.APIResourceList{GroupVersion: "networking.k8s.io/v1",
						APIResources: []metav1.APIResource{{Name: "ingressclasses", Kind: "IngressClass", Namespaced: false}}})
				case "/api/v1/namespaces/ingress-system/configmaps/edge-check":
					w.WriteHeader(http.StatusNotFound)
					respond(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: 404})
				case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
					respond(&authv1.SelfSubjectAccessReview{Status: authv1.SubjectAccessReviewStatus{Allowed: scenario != "forbidden"}})
				case "/api/v1/namespaces/ingress-system/configmaps":
					if r.Method != http.MethodPost || r.URL.Query().Get("dryRun") != "All" {
						t.Errorf("preflight attempted a mutation without dryRun=All: %s", r.Method)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					dryRuns++
					if scenario == "dry-run-rejected" {
						w.WriteHeader(http.StatusForbidden)
						respond(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403})
						return
					}
					var obj map[string]any
					if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
						t.Error(err)
					}
					respond(obj)
				default:
					t.Errorf("unexpected preflight API request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			kc, err := clientsFromRest(&rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			fixture := &chart.Chart{Metadata: &chart.Metadata{Name: "traefik", Version: "41.7.1", KubeVersion: ">=1.21.0-0"},
				Templates: []*chart.File{{Name: "templates/check.yaml", Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: edge-check\n  namespace: ingress-system\n")}}}
			err = prepareVerifiedComponentChart(context.Background(), kc, fixture, map[string]any{}, componentInstallRequest{Component: "traefik", Namespace: "ingress-system", ReleaseName: "edge", Version: "41.7.1"})
			if (err == nil) != (scenario == "ready") {
				t.Fatalf("scenario %s preflight result: %v", scenario, err)
			}
			if scenario == "ready" && dryRuns != 1 {
				t.Fatalf("dry-run count = %d, want 1", dryRuns)
			}
		})
	}
}

func TestComponentCRDPreflightThenServerValidationAndHelmFixture(t *testing.T) {
	for _, scenario := range []string{"ready", "crd-forbidden", "hook-forbidden", "future-invalid", "post-crd-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			established, writes, customDryRuns := false, 0, 0
			var storedCRD map[string]any
			crd := componentTestCRD()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				respond := func(v any) { capabilityTestJSON(t, w, v) }
				failed := func(reason metav1.StatusReason, code int) {
					w.WriteHeader(code)
					respond(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: reason, Code: int32(code)})
				}
				switch r.URL.Path {
				case "/readyz":
					_, _ = w.Write([]byte("ok"))
				case "/version":
					respond(map[string]string{"gitVersion": "v1.34.0"})
				case "/api":
					respond(&metav1.APIVersions{Versions: []string{"v1"}})
				case "/apis":
					groups := []metav1.APIGroup{}
					names := []string{"apiextensions.k8s.io", "batch"}
					if established {
						names = append(names, "example.test")
					}
					for _, name := range names {
						gv := metav1.GroupVersionForDiscovery{GroupVersion: name + "/v1", Version: "v1"}
						groups = append(groups, metav1.APIGroup{Name: name, Versions: []metav1.GroupVersionForDiscovery{gv}, PreferredVersion: gv})
					}
					respond(&metav1.APIGroupList{Groups: groups})
				case "/api/v1":
					respond(&metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "configmaps", Kind: "ConfigMap", Namespaced: true}}})
				case "/apis/apiextensions.k8s.io/v1":
					respond(&metav1.APIResourceList{GroupVersion: "apiextensions.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "customresourcedefinitions", Kind: "CustomResourceDefinition"}}})
				case "/apis/batch/v1":
					respond(&metav1.APIResourceList{GroupVersion: "batch/v1", APIResources: []metav1.APIResource{{Name: "jobs", Kind: "Job", Namespaced: true}}})
				case "/apis/example.test/v1":
					respond(&metav1.APIResourceList{GroupVersion: "example.test/v1", APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}}})
				case "/api/v1/namespaces/test":
					respond(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
				case "/api/v1/namespaces/test/secrets":
					respond(&corev1.SecretList{Items: []corev1.Secret{}})
				case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/widgets.example.test":
					if !established {
						failed(metav1.StatusReasonNotFound, 404)
						return
					}
					current := (&unstructured.Unstructured{Object: storedCRD}).DeepCopy()
					current.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}}
					respond(current)
				case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
					var review authv1.SelfSubjectAccessReview
					if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
						t.Error(err)
					}
					resource := review.Spec.ResourceAttributes.Resource
					allowed := !(scenario == "crd-forbidden" && resource == "customresourcedefinitions" || scenario == "hook-forbidden" && resource == "jobs")
					respond(&authv1.SelfSubjectAccessReview{Status: authv1.SubjectAccessReviewStatus{Allowed: allowed}})
				case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "/api/v1/namespaces/test/configmaps", "/apis/batch/v1/namespaces/test/jobs", "/apis/example.test/v1/namespaces/test/widgets":
					if r.Method != http.MethodPost {
						t.Errorf("unexpected method: %s", r.Method)
						failed(metav1.StatusReasonBadRequest, 400)
						return
					}
					if r.URL.Query().Get("dryRun") != "All" {
						if r.URL.Path != "/apis/apiextensions.k8s.io/v1/customresourcedefinitions" {
							t.Error("workload write occurred during preflight")
						}
						writes++
						established = true
					}
					if strings.HasSuffix(r.URL.Path, "/widgets") {
						customDryRuns++
						if !established {
							t.Error("future custom resource submitted before CRD establishment")
						}
						if scenario == "post-crd-rejected" {
							failed(metav1.StatusReasonForbidden, 403)
							return
						}
					}
					var obj map[string]any
					if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
						t.Error(err)
					}
					if r.URL.Path == "/apis/apiextensions.k8s.io/v1/customresourcedefinitions" {
						storedCRD = obj
					}
					respond(obj)
				default:
					if r.Method == http.MethodGet && (strings.Contains(r.URL.Path, "/widgets/") || strings.Contains(r.URL.Path, "/configmaps/") || strings.Contains(r.URL.Path, "/jobs/")) {
						failed(metav1.StatusReasonNotFound, 404)
						return
					}
					t.Errorf("unexpected staged API request: %s %s", r.Method, r.URL.Path)
					failed(metav1.StatusReasonNotFound, 404)
				}
			}))
			defer server.Close()
			kc, err := clientsFromRest(&rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}})
			if err != nil {
				t.Fatal(err)
			}
			def, _ := componentDefinition("argocd")
			crdData, _ := json.Marshal(crd.Object)
			size := 1
			if scenario == "future-invalid" {
				size = 0
			}
			ch := &chart.Chart{Metadata: &chart.Metadata{Name: def.Chart, Version: def.Versions[0]},
				Files: []*chart.File{{Name: "crds/widget.json", Data: crdData}},
				Templates: []*chart.File{
					{Name: "templates/custom.yaml", Data: []byte(fmt.Sprintf("apiVersion: example.test/v1\nkind: Widget\nmetadata:\n  name: example\nspec:\n  size: %d\n", size))},
					{Name: "templates/hook.yaml", Data: []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: argo-redis-secret-init\n  annotations:\n    helm.sh/hook: pre-install,pre-upgrade\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n      - name: init\n        image: example.test/fixture\n")},
				}}
			in := componentInstallRequest{Component: "argocd", Namespace: "test", ReleaseName: "argo", Version: def.Versions[0]}
			err = prepareVerifiedComponentChart(context.Background(), kc, ch, map[string]any{}, in)
			if writes != 0 || customDryRuns != 0 {
				t.Fatal("read-only preflight performed mutations or future API calls")
			}
			if scenario == "crd-forbidden" || scenario == "hook-forbidden" || scenario == "future-invalid" {
				if err == nil {
					t.Fatal("unsafe preflight accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = prepareComponentCRDInstall(context.Background(), kc, ch, map[string]any{}, in)
			if writes != 1 || customDryRuns != 1 {
				t.Fatalf("CRD writes=%d custom dry-runs=%d, result=%v", writes, customDryRuns, err)
			}
			if scenario == "post-crd-rejected" {
				if err == nil {
					t.Fatal("post-CRD admission rejection ignored")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg := &action.Configuration{Releases: storage.Init(driver.NewMemory()), KubeClient: &helmfake.PrintingKubeClient{Out: io.Discard}, Capabilities: chartutil.DefaultCapabilities, Log: func(string, ...interface{}) {}}
			status, err := runVerifiedComponentInstallWithConfig(context.Background(), cfg, ch, map[string]any{}, in)
			if err != nil || status != "deployed" {
				t.Fatalf("staged SDK fixture result=%q err=%v", status, err)
			}
		})
	}
}

func TestComponentReleaseAssetRedirectValidation(t *testing.T) {
	accepted, _ := url.Parse("https://release-assets.githubusercontent.com/github-production-release-asset/123/chart.tgz?sp=r&sv=2024-11-04&sr=b&se=2030-01-01T00%3A00%3A00Z&sig=redacted")
	if err := componentReleaseAssetURL(accepted); err != nil {
		t.Fatalf("accept canonical signed GitHub asset URL: %v", err)
	}
	for _, raw := range []string{
		"https://127.0.0.1/github-production-release-asset/123/chart.tgz?sp=r&se=x&sig=x",
		"https://release-assets.githubusercontent.com.evil.test/github-production-release-asset/123/chart.tgz?sp=r&se=x&sig=x",
		"https://release-assets.githubusercontent.com/github-production-release-asset/../chart.tgz?sp=r&se=x&sig=x",
		"https://release-assets.githubusercontent.com/github-production-release-asset/123/chart.tgz?token=secret",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := componentReleaseAssetURL(u); err == nil {
			t.Fatalf("unsafe release URL accepted: %s", u.Host+u.Path)
		}
	}
}

func TestComponentPublicAddressValidation(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !componentGlobalUnicast(net.ParseIP(raw)) {
			t.Fatalf("public address rejected: %s", raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "::1", "fc00::1"} {
		if componentGlobalUnicast(net.ParseIP(raw)) {
			t.Fatalf("non-public address accepted: %s", raw)
		}
	}
}

func TestComponentOperationPostgresClaimAndAudit(t *testing.T) {
	dsn := os.Getenv("MATRIX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MATRIX_TEST_DATABASE_URL to run PostgreSQL claim/audit integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.cluster_component_operations') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatal("cluster_component_operations migration must be applied to the isolated test database")
	}
	clusterID := os.Getenv("MATRIX_TEST_CLUSTER_ID")
	if clusterID == "" {
		t.Skip("set MATRIX_TEST_CLUSTER_ID to a disposable existing cluster row")
	}
	var env, clusterName string
	if err := pool.QueryRow(ctx, `SELECT env,name FROM cluster_clusters WHERE id=$1`, clusterID).Scan(&env, &clusterName); err != nil {
		t.Fatalf("read test cluster scope: %v", err)
	}
	var actor int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&actor); err != nil {
		t.Fatalf("read test actor: %v", err)
	}
	id := uuid.NewString()
	release := "claim-" + strings.ReplaceAll(id[:8], "-", "")
	_, err = pool.Exec(ctx, `INSERT INTO cluster_component_operations
		(id,cluster_id,cluster_name,environment,component,namespace,release_name,chart_version,chart_digest,config_encrypted,status,created_by)
		VALUES ($1,$2,$3,$4,'traefik','default',$5,'4.11.3',$6,'test-ciphertext','preflight',$7)`,
		id, clusterID, clusterName, env, release, strings.Repeat("a", 64), actor)
	if err != nil {
		t.Fatalf("insert isolated operation fixture: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM cluster_component_operations WHERE id=$1`, id)
	type claimResult struct {
		rows int64
		err  error
	}
	results := make(chan claimResult, 2)
	for range 2 {
		go func() {
			tag, err := pool.Exec(ctx, `UPDATE cluster_component_operations SET status='running',started_at=now()
				WHERE id=$1 AND cluster_id=$2 AND environment=$3 AND status='preflight'`, id, clusterID, env)
			if err != nil {
				results <- claimResult{err: err}
				return
			}
			results <- claimResult{rows: tag.RowsAffected()}
		}()
	}
	successfulClaims := int64(0)
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatalf("claim operation: %v", got.err)
		}
		successfulClaims += got.rows
	}
	if successfulClaims != 1 {
		t.Fatalf("concurrent claim rows affected = %d, want exactly one", successfulClaims)
	}
	operation := componentOperation{ID: id, ClusterID: parseComponentTestInt64(t, clusterID), Environment: env,
		Component: "ingress-nginx", Namespace: "default", ReleaseName: release}
	mod := &Module{d: &app.Deps{DB: pool}}
	cl := &Cluster{ID: operation.ClusterID, Env: env}
	if err := mod.finalizeComponentSuccess(ctx, id, cl, operation.Component, operation.Namespace, operation.ReleaseName, "deployed"); err != nil {
		t.Fatalf("transactional audit and success state: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM cluster_component_operations WHERE id=$1`, id).Scan(&status); err != nil || status != "succeeded" {
		t.Fatalf("operation state after audit = %q, err=%v", status, err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE detail->>'operation_id'=$1`, id).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit rows = %d, err=%v", auditCount, err)
	}
	defer pool.Exec(ctx, `DELETE FROM audit_logs WHERE detail->>'operation_id'=$1`, id)
}

func parseComponentTestInt64(t *testing.T, value string) int64 {
	t.Helper()
	result, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestComponentEncryptedSnapshotAAD(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.EncryptBytes([]byte("config"), []byte("cluster-component:op-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipher.DecryptBytes(encrypted, []byte("cluster-component:op-b")); err == nil {
		t.Fatal("encrypted approval config must not be transferable between operations")
	}
}

func TestComponentRequestRejectsUnsupportedAndInvalidInputs(t *testing.T) {
	if _, _, err := validateComponentRequest("ingress-nginx", componentInstallRequest{
		Namespace: "ingress-system", ReleaseName: "edge-ingress", Version: "41.7.1",
		Values: map[string]string{"controller.extraArgs": "unsafe"},
	}); err == nil {
		t.Fatal("unlisted chart values must be rejected")
	}
	if _, _, err := validateComponentRequest("not-curated", componentInstallRequest{Version: "1.0.0"}); err == nil {
		t.Fatal("unknown component must fail closed")
	}
	if _, _, err := validateComponentRequest("ingress-nginx", componentInstallRequest{
		Namespace: "ingress-system", ReleaseName: "edge-ingress", Version: "41.7.1",
	}); err == nil {
		t.Fatal("retired ingress-nginx chart must fail closed")
	}
	if _, _, err := validateComponentRequest("ingress-nginx", componentInstallRequest{
		Namespace: "../bad", ReleaseName: "edge-ingress", Version: "41.7.1",
	}); err == nil {
		t.Fatal("invalid namespace must be rejected by stored-config validation")
	}
}
