package cicd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// fakeClusters is an in-memory app.ClusterAccess.
type fakeClusters struct {
	refs          map[int64]app.ClusterRef
	clusterErr    error
	restErr       error
	kubeconfig    map[int64][]byte
	restCalls     int
	kubeconfigCnt int
}

func (f *fakeClusters) Cluster(_ context.Context, id int64) (app.ClusterRef, error) {
	if f.clusterErr != nil {
		return app.ClusterRef{}, f.clusterErr
	}
	ref, ok := f.refs[id]
	if !ok {
		return app.ClusterRef{}, app.ErrClusterNotFound
	}
	return ref, nil
}

func (f *fakeClusters) RESTConfig(_ context.Context, id int64) (*rest.Config, error) {
	f.restCalls++
	if f.restErr != nil {
		return nil, f.restErr
	}
	if _, ok := f.refs[id]; !ok {
		return nil, app.ErrClusterNotFound
	}
	return &rest.Config{Host: "https://cluster.invalid", Timeout: 30 * time.Second}, nil
}

func (f *fakeClusters) Kubeconfig(_ context.Context, id int64) ([]byte, error) {
	f.kubeconfigCnt++
	if kc, ok := f.kubeconfig[id]; ok {
		return kc, nil
	}
	return nil, app.ErrNoKubeconfig
}

func newClusterFixture() *fakeClusters {
	return &fakeClusters{refs: map[int64]app.ClusterRef{
		3: {ID: 3, Name: "prod-a", Env: "prod", ConnectMode: app.ClusterConnectKubeconfig},
		4: {ID: 4, Name: "edge", Env: "prod", ConnectMode: app.ClusterConnectAgent},
		5: {ID: 5, Name: "dev-a", Env: "dev", ConnectMode: app.ClusterConnectKubeconfig},
	}, kubeconfig: map[int64][]byte{3: []byte("apiVersion: v1\nkind: Config\n")}}
}

// targetModule wires a module with fake target rows, a fake cluster registry
// and seams that record which clientset path was taken.
func targetModule(t *testing.T, clusters app.ClusterAccess, rows ...*targetRow) (*Module, *string, *[]*rest.Config) {
	t.Helper()
	path := ""
	var configs []*rest.Config
	byID := map[int64]*targetRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	m := New()
	m.d = &app.Deps{Crypto: argoTestCipher(t), Clusters: clusters}
	m.loadTargetRow = func(_ context.Context, id int64) (*targetRow, error) {
		if r, ok := byID[id]; ok {
			cp := *r
			return &cp, nil
		}
		return nil, httpx.ErrNotFound
	}
	m.kube = func([]byte) (kubernetes.Interface, error) { path = "legacy"; return fake.NewSimpleClientset(), nil }
	m.restKube = func(cfg *rest.Config) (kubernetes.Interface, error) {
		path = "cluster"
		configs = append(configs, cfg)
		return fake.NewSimpleClientset(), nil
	}
	return m, &path, &configs
}

func legacyRow(t *testing.T, m *Module, id int64, env string) *targetRow {
	t.Helper()
	enc, err := m.d.Crypto.Encrypt("apiVersion: v1\nkind: Config\n")
	if err != nil {
		t.Fatal(err)
	}
	return &targetRow{ID: id, Name: "legacy", Namespace: "ci", Env: env, KubeconfigEnc: enc, UpdatedAt: time.Unix(100, 0)}
}

func statusOf(err error) int {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestTargetClusterMigrationParses(t *testing.T) {
	migrations, err := store.ParseMigrations(New().Migrations())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.Version != 6 {
			continue
		}
		if m.Name != "target_cluster" {
			t.Fatalf("migration 6 name = %q", m.Name)
		}
		for _, want := range []string{
			"ADD COLUMN IF NOT EXISTS cluster_id BIGINT REFERENCES cluster_clusters(id)",
			"CREATE INDEX IF NOT EXISTS cicd_targets_cluster_idx",
			"ALTER COLUMN kubeconfig_enc DROP NOT NULL",
			"DROP CONSTRAINT IF EXISTS cicd_targets_cluster_or_kubeconfig_check",
			"CHECK ((cluster_id IS NOT NULL) <> (COALESCE(kubeconfig_enc, '') <> ''))",
		} {
			if !strings.Contains(m.SQL, want) {
				t.Fatalf("migration 6 is missing %q", want)
			}
		}
		if strings.Contains(strings.ToUpper(m.SQL), "CASCADE") {
			t.Fatal("cluster reference must not cascade deletes to targets")
		}
		return
	}
	t.Fatal("0006_target_cluster migration is missing")
}

func TestTargetClientPicksClusterOrLegacyPath(t *testing.T) {
	clusters := newClusterFixture()
	m, path, configs := targetModule(t, clusters, &targetRow{ID: 1, Name: "linked", Namespace: "matrix-cicd", Env: "prod", ClusterID: 3})
	conn, err := m.targetClient(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if *path != "cluster" || conn.cluster == nil || conn.cluster.ID != 3 || conn.namespace != "matrix-cicd" || conn.legacyKC != nil {
		t.Fatalf("linked target resolved wrongly: path=%s conn=%+v", *path, conn)
	}
	if len(*configs) != 1 || (*configs)[0].Timeout != 0 {
		t.Fatal("cluster rest config must have Timeout=0 for long-lived log streams")
	}

	m2, path2, _ := targetModule(t, clusters)
	row := legacyRow(t, m2, 2, "prod")
	m2.loadTargetRow = func(context.Context, int64) (*targetRow, error) { cp := *row; return &cp, nil }
	conn, err = m2.targetClient(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if *path2 != "legacy" || conn.cluster != nil || len(conn.legacyKC) == 0 || conn.namespace != "ci" {
		t.Fatalf("legacy target resolved wrongly: path=%s", *path2)
	}
	if clusters.restCalls != 1 {
		t.Fatalf("legacy target must not use the cluster registry, rest calls=%d", clusters.restCalls)
	}
}

func TestTargetClientFailsClosed(t *testing.T) {
	linked := &targetRow{ID: 1, Name: "linked", Namespace: "ci", Env: "prod", ClusterID: 3}

	t.Run("env mismatch", func(t *testing.T) {
		clusters := newClusterFixture()
		clusters.refs[3] = app.ClusterRef{ID: 3, Name: "prod-a", Env: "staging"}
		m, path, _ := targetModule(t, clusters, linked)
		_, err := m.targetClient(context.Background(), 1)
		if statusOf(err) != http.StatusConflict || *path != "" || clusters.restCalls != 0 {
			t.Fatalf("env mismatch must fail before connecting: err=%v path=%q rest=%d", err, *path, clusters.restCalls)
		}
		if err := m.validateDeploymentTarget(context.Background(), &EnvTarget{Name: "prod", TargetID: 1}); statusOf(err) != http.StatusConflict {
			t.Fatalf("deploy validation must reject a cluster that moved environments: %v", err)
		}
	})
	t.Run("nil cluster registry", func(t *testing.T) {
		m, path, _ := targetModule(t, nil, linked)
		_, err := m.targetClient(context.Background(), 1)
		if statusOf(err) != http.StatusServiceUnavailable || *path != "" {
			t.Fatalf("nil Deps.Clusters must fail closed with 503: %v", err)
		}
	})
	t.Run("offline agent", func(t *testing.T) {
		clusters := newClusterFixture()
		clusters.restErr = app.ErrClusterOffline
		m, _, _ := targetModule(t, clusters, linked)
		_, err := m.targetClient(context.Background(), 1)
		if !errors.Is(err, app.ErrClusterOffline) || statusOf(err) != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "Agent 未连接") {
			t.Fatalf("offline cluster error must surface: %v", err)
		}
	})
	t.Run("cluster deleted", func(t *testing.T) {
		m, _, _ := targetModule(t, &fakeClusters{refs: map[int64]app.ClusterRef{}}, linked)
		if _, err := m.targetClient(context.Background(), 1); statusOf(err) != http.StatusConflict {
			t.Fatalf("missing cluster must fail closed: %v", err)
		}
	})
}

func TestDeploymentTargetKeepsLegacyRowsAndChecksLinkedEnv(t *testing.T) {
	clusters := newClusterFixture()
	m, _, _ := targetModule(t, clusters, &targetRow{ID: 1, Name: "linked", Env: "prod", ClusterID: 3})
	if err := m.validateDeploymentTarget(context.Background(), &EnvTarget{Name: "prod", TargetID: 1}); err != nil {
		t.Fatalf("matching linked target rejected: %v", err)
	}
	if err := m.validateDeploymentTarget(context.Background(), &EnvTarget{Name: "dev", TargetID: 1}); statusOf(err) != http.StatusConflict {
		t.Fatalf("application env mismatch accepted: %v", err)
	}
	legacy := legacyRow(t, m, 2, "prod")
	m2, _, _ := targetModule(t, nil, legacy)
	if err := m2.validateDeploymentTarget(context.Background(), &EnvTarget{Name: "prod", TargetID: 2}); err != nil {
		t.Fatalf("legacy target must keep working without the cluster registry: %v", err)
	}
}

func postTarget(t *testing.T, m *Module, p *rbac.Principal, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { rbac.SetPrincipal(c, p) })
	r.POST("/cicd/targets", m.createTarget)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/cicd/targets", strings.NewReader(body)))
	return w.Code, w.Body.String()
}

func TestCreateTargetValidation(t *testing.T) {
	m, _, _ := targetModule(t, newClusterFixture())
	all := &rbac.Principal{UserID: 1, Username: "admin"}
	devOnly := &rbac.Principal{UserID: 2, Username: "dev", EnvScope: []string{"dev"}}
	cases := []struct {
		name   string
		p      *rbac.Principal
		body   string
		status int
		match  string
	}{
		{"kubeconfig rejected", all, `{"name":"t","cluster_id":3,"kubeconfig":"apiVersion: v1"}`, 400, "集群管理"},
		{"kubeconfig without cluster rejected", all, `{"name":"t","kubeconfig":"apiVersion: v1"}`, 400, "集群管理"},
		{"cluster required", all, `{"name":"t"}`, 400, "cluster_id"},
		{"unknown cluster", all, `{"name":"t","cluster_id":99}`, 400, "未注册"},
		{"env mismatch", all, `{"name":"t","cluster_id":3,"env":"dev"}`, 400, "必须与集群"},
		{"env out of scope", devOnly, `{"name":"t","cluster_id":3}`, 403, "prod"},
		{"bad namespace", all, `{"name":"t","cluster_id":3,"namespace":"Bad_NS"}`, 400, "namespace"},
		// Valid input passes validation and reaches the approval gate, which
		// is not configured here and fails closed.
		{"valid reaches gate", devOnly, `{"name":"t","cluster_id":5}`, 503, "审批"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postTarget(t, m, tc.p, tc.body)
			if code != tc.status || !strings.Contains(body, tc.match) {
				t.Fatalf("status=%d body=%s, want %d containing %q", code, body, tc.status, tc.match)
			}
			if strings.Contains(body, "apiVersion") {
				t.Fatal("response echoed kubeconfig input")
			}
		})
	}

	in := &targetIn{Name: "t", ClusterID: 5}
	change, err := m.prepareTargetCreate(context.Background(), devOnly, in)
	if err != nil {
		t.Fatal(err)
	}
	if in.Env != "dev" || in.Namespace != defaultRunnerNamespace || change.Kind != "target.create" || change.Envs[0] != "dev" {
		t.Fatalf("env must default to the cluster env: %+v %+v", in, change)
	}
	m2, _, _ := targetModule(t, nil)
	if _, err := m2.prepareTargetCreate(context.Background(), all, &targetIn{Name: "t", ClusterID: 3}); statusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("create without cluster registry must fail closed: %v", err)
	}
}

func TestPrepareTargetUpdate(t *testing.T) {
	m, _, _ := targetModule(t, newClusterFixture())
	all := &rbac.Principal{UserID: 1}
	legacy := legacyRow(t, m, 7, "prod")

	change, err := m.prepareTargetUpdate(context.Background(), all, legacy, &targetUpdateIn{ClusterID: 4})
	if err != nil {
		t.Fatal(err)
	}
	if change.Kind != "target.update" || change.ID != 7 || change.Target.ClusterID != 4 || change.Target.Env != "prod" ||
		change.Target.Name != "legacy" || change.Target.Namespace != "ci" || !change.ExpectedUpdatedAt.Equal(legacy.UpdatedAt) {
		t.Fatalf("unexpected link change: %+v %+v", change, change.Target)
	}
	if _, err := m.prepareTargetUpdate(context.Background(), all, legacy, &targetUpdateIn{ClusterID: 5}); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("linking to a cluster of another env must fail: %v", err)
	}
	if _, err := m.prepareTargetUpdate(context.Background(), all, legacy, &targetUpdateIn{ClusterID: 3, Kubeconfig: "apiVersion: v1"}); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("kubeconfig on update must be rejected: %v", err)
	}
	ns := "other"
	change, err = m.prepareTargetUpdate(context.Background(), all, legacy, &targetUpdateIn{Namespace: &ns})
	if err != nil || change.Target.ClusterID != 0 || change.Target.Namespace != "other" {
		t.Fatalf("namespace-only update of a legacy target: %v %+v", err, change.Target)
	}
	unbound := legacyRow(t, m, 8, "")
	change, err = m.prepareTargetUpdate(context.Background(), all, unbound, &targetUpdateIn{ClusterID: 5})
	if err != nil || change.Target.Env != "dev" {
		t.Fatalf("legacy target without env must take the cluster env: %v %+v", err, change.Target)
	}
	devOnly := &rbac.Principal{UserID: 2, EnvScope: []string{"dev"}}
	if _, err := m.prepareTargetUpdate(context.Background(), devOnly, legacy, &targetUpdateIn{ClusterID: 3}); statusOf(err) != http.StatusForbidden {
		t.Fatalf("update of a target outside the caller env must be forbidden: %v", err)
	}
	linked := &targetRow{ID: 9, Name: "linked", Namespace: "ci", Env: "prod", ClusterID: 3, UpdatedAt: time.Unix(5, 0)}
	change, err = m.prepareTargetUpdate(context.Background(), all, linked, &targetUpdateIn{Namespace: &ns})
	if err != nil || change.Target.ClusterID != 3 || change.Target.Kubeconfig != "" {
		t.Fatalf("namespace update keeps the cluster link: %v %+v", err, change.Target)
	}
}

func TestStagedTargetIsRevalidatedAtApproval(t *testing.T) {
	clusters := newClusterFixture()
	legacy := &targetRow{ID: 7, Name: "legacy", Namespace: "ci", Env: "prod", KubeconfigEnc: "enc", UpdatedAt: time.Unix(100, 0)}
	m, _, _ := targetModule(t, clusters, legacy)
	ctx := context.Background()
	create := cicdConfigMutation{Kind: "target.create", Target: &targetIn{Name: "t", ClusterID: 3, Namespace: "ci", Env: "prod"}, Envs: []string{"prod"}}
	if err := m.validateConfigChange(ctx, create); err != nil {
		t.Fatalf("valid staged create rejected: %v", err)
	}
	stale := create
	stale.Target = &targetIn{Name: "t", ClusterID: 3, Namespace: "ci", Env: "prod", Kubeconfig: "apiVersion: v1"}
	if err := m.validateConfigChange(ctx, stale); err == nil {
		t.Fatal("a staged kubeconfig (pre-migration change) must be rejected")
	}
	updated := legacy.UpdatedAt
	link := cicdConfigMutation{Kind: "target.update", ID: 7, Target: &targetIn{Name: "legacy", ClusterID: 4, Namespace: "ci", Env: "prod"},
		Envs: []string{"prod"}, ExpectedUpdatedAt: &updated}
	if err := m.validateConfigChange(ctx, link); err != nil {
		t.Fatalf("valid staged link rejected: %v", err)
	}
	moved := time.Unix(200, 0)
	conflict := link
	conflict.ExpectedUpdatedAt = &moved
	if err := m.validateConfigChange(ctx, conflict); statusOf(err) != http.StatusConflict {
		t.Fatalf("target edited while pending must conflict: %v", err)
	}

	clusters.refs[3] = app.ClusterRef{ID: 3, Name: "prod-a", Env: "staging"}
	if err := m.validateConfigChange(ctx, create); err == nil {
		t.Fatal("cluster env change while pending must be rejected")
	}
	delete(clusters.refs, 4)
	if err := m.validateConfigChange(ctx, link); err == nil {
		t.Fatal("cluster removed while pending must be rejected")
	}
}

func TestHelmAuthForTargets(t *testing.T) {
	clusters := newClusterFixture()
	m, _, _ := targetModule(t, clusters)
	auth, err := m.helmAuthFor(context.Background(), &targetConn{row: &targetRow{ClusterID: 3}})
	if err != nil || len(auth.kubeconfig) == 0 || auth.serviceAccount != "" {
		t.Fatalf("kubeconfig cluster: %v %+v", err, auth.serviceAccount)
	}
	auth, err = m.helmAuthFor(context.Background(), &targetConn{row: &targetRow{ClusterID: 4}})
	if err != nil || len(auth.kubeconfig) != 0 || auth.serviceAccount != DeployerServiceAccount {
		t.Fatalf("agent cluster must use the deployer ServiceAccount: %v %+v", err, auth)
	}
	auth, err = m.helmAuthFor(context.Background(), &targetConn{row: &targetRow{KubeconfigEnc: "x"}, legacyKC: []byte("legacy")})
	if err != nil || string(auth.kubeconfig) != "legacy" || clusters.kubeconfigCnt != 2 {
		t.Fatalf("legacy target keeps its kubeconfig: %v", err)
	}
	m2, _, _ := targetModule(t, nil)
	if _, err := m2.helmAuthFor(context.Background(), &targetConn{row: &targetRow{ClusterID: 4}}); statusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("nil cluster registry must fail closed: %v", err)
	}
}

func helmEnv() *EnvTarget {
	return &EnvTarget{Name: "prod", TargetID: 1, Namespace: "apps", Helm: &HelmSpec{Release: "api", Chart: "oci://reg/charts/api"}}
}

// captureJobs records created Jobs and Secrets on a fake clientset.
func captureJobs(cs *fake.Clientset) (*[]*batchv1.Job, *[]*corev1.Secret) {
	var jobs []*batchv1.Job
	var secrets []*corev1.Secret
	cs.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		jobs = append(jobs, a.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy())
		return false, nil, nil
	})
	cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		secrets = append(secrets, a.(k8stesting.CreateAction).GetObject().(*corev1.Secret).DeepCopy())
		return false, nil, nil
	})
	return &jobs, &secrets
}

func TestHelmDeployAgentClusterUsesServiceAccount(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: DeployerServiceAccount, Namespace: "matrix-cicd"}})
	jobs, secrets := captureJobs(cs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stop runJob right after the Job is created
	err := helmDeploy(ctx, cs, "matrix-cicd", helmAuth{serviceAccount: DeployerServiceAccount}, helmEnv(), "reg/api:v2", 12, time.Minute, func(string, ...any) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancelled run, got %v", err)
	}
	if len(*jobs) != 1 || len(*secrets) != 0 {
		t.Fatalf("agent helm deploy must create one Job and no kubeconfig Secret: jobs=%d secrets=%d", len(*jobs), len(*secrets))
	}
	spec := (*jobs)[0].Spec.Template.Spec
	if spec.ServiceAccountName != DeployerServiceAccount || spec.AutomountServiceAccountToken == nil || !*spec.AutomountServiceAccountToken {
		t.Fatalf("helm Job must run as %s with its token: %+v", DeployerServiceAccount, spec)
	}
	if len(spec.Volumes) != 0 {
		t.Fatalf("agent helm Job must not mount a kubeconfig volume: %+v", spec.Volumes)
	}
	for _, c := range spec.Containers {
		for _, e := range c.Env {
			if e.Name == "KUBECONFIG" {
				t.Fatal("agent helm Job must not set KUBECONFIG")
			}
		}
		if len(c.VolumeMounts) != 0 {
			t.Fatalf("agent helm Job must not mount volumes: %+v", c.VolumeMounts)
		}
	}
}

func TestHelmDeployMissingServiceAccountFailsBeforeJob(t *testing.T) {
	cs := fake.NewSimpleClientset()
	jobs, secrets := captureJobs(cs)
	err := helmDeploy(context.Background(), cs, "matrix-cicd", helmAuth{serviceAccount: DeployerServiceAccount}, helmEnv(), "reg/api:v2", 12, time.Minute, func(string, ...any) {})
	if err == nil || err.Error() != msgDeployerSAMissing {
		t.Fatalf("missing ServiceAccount error = %v", err)
	}
	if len(*jobs) != 0 || len(*secrets) != 0 {
		t.Fatal("no Job or Secret may be created when the ServiceAccount is missing")
	}
	if err := helmDeploy(context.Background(), cs, "matrix-cicd", helmAuth{}, helmEnv(), "reg/api:v2", 12, time.Minute, func(string, ...any) {}); err == nil || len(*jobs) != 0 {
		t.Fatal("helm deploy without any credential must fail before creating a Job")
	}
}

func TestHelmDeployKubeconfigClusterMountsSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	jobs, secrets := captureJobs(cs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = helmDeploy(ctx, cs, "matrix-cicd", helmAuth{kubeconfig: []byte("apiVersion: v1")}, helmEnv(), "reg/api:v2", 12, time.Minute, func(string, ...any) {})
	if len(*jobs) != 1 || len(*secrets) != 1 {
		t.Fatalf("kubeconfig helm deploy: jobs=%d secrets=%d", len(*jobs), len(*secrets))
	}
	spec := (*jobs)[0].Spec.Template.Spec
	if spec.ServiceAccountName != "" || len(spec.Volumes) != 1 || spec.Volumes[0].Secret == nil || spec.Volumes[0].Secret.SecretName != (*secrets)[0].Name {
		t.Fatalf("kubeconfig helm Job must mount the short-lived Secret: %+v", spec)
	}
	if _, err := cs.CoreV1().Secrets("matrix-cicd").Get(context.Background(), (*secrets)[0].Name, metav1.GetOptions{}); err == nil {
		t.Fatal("kubeconfig Secret must be deleted after the Job")
	}
}

func TestTargetListNeverLeaksCredentials(t *testing.T) {
	for i := 0; i < reflect.TypeOf(targetListRow{}).NumField(); i++ {
		if name := strings.ToLower(reflect.TypeOf(targetListRow{}).Field(i).Name); strings.Contains(name, "kube") || strings.Contains(name, "enc") {
			t.Fatalf("list rows must not carry credential columns: %s", name)
		}
	}
	at := time.Unix(1, 0).UTC()
	legacy := targetListItem(targetListRow{ID: 1, Name: "old", Namespace: "ci", Env: "prod", Legacy: true, CreatedAt: at}, nil)
	ref := app.ClusterRef{ID: 4, Name: "edge", Env: "prod", ConnectMode: app.ClusterConnectAgent}
	linked := targetListItem(targetListRow{ID: 2, Name: "new", Namespace: "ci", Env: "prod", ClusterID: 4, CreatedAt: at}, &ref)
	unreadable := targetListItem(targetListRow{ID: 3, Name: "x", ClusterID: 9, CreatedAt: at}, nil)

	raw, _ := json.Marshal([]gin.H{legacy, linked, unreadable})
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got[0]["legacy_kubeconfig"] != true || got[0]["kubeconfig"] != "********" || got[0]["cluster_id"] != nil {
		t.Fatalf("legacy row: %v", got[0])
	}
	if got[1]["legacy_kubeconfig"] != false || got[1]["cluster_name"] != "edge" || got[1]["connect_mode"] != "agent" || got[1]["cluster_id"] != float64(4) {
		t.Fatalf("linked row: %v", got[1])
	}
	if _, ok := got[1]["kubeconfig"]; ok {
		t.Fatal("linked rows must not carry a kubeconfig field")
	}
	if got[2]["cluster_name"] != "" || got[2]["cluster_id"] != float64(9) {
		t.Fatalf("failed cluster lookup must leave the name empty: %v", got[2])
	}
	if strings.Contains(string(raw), "apiVersion") || strings.Contains(string(raw), "token") {
		t.Fatal("credential material leaked into the target list")
	}
}
