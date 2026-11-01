package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeFake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testProjectResources(name, value string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceName(name): resource.MustParse(value)}
}

func testProjectQuotaMutation() projectQuotaMutation {
	return projectQuotaMutation{ClusterID: 1, Cluster: "test", Action: "quota_create", Quota: &projectQuotaParams{
		Namespace: "apps", Name: "budget", Spec: &corev1.ResourceQuotaSpec{Hard: testProjectResources("requests.cpu", "2")},
	}}
}

func TestResourceQuotaStrictInputAndValidation(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"spec":{"hard":{"requests.cpu":"bad"}}}`, `{"spec":{"hard":{"cpu":"1"}},"unknown":"private-marker"}`,
		`{"spec":{"hard":{"cpu":"1"}}} {}`, `{"spec":{"hard":{"cpu":"1"},"unknown":"x"}}`,
	} {
		var input projectQuotaParams
		err := decodeProjectInput([]byte(raw), &input)
		if err == nil {
			err = validateProjectQuotaMutation(projectQuotaMutation{Action: "quota_create", Quota: &input})
		}
		if err == nil {
			t.Fatalf("invalid input accepted: %s", raw)
		}
		if strings.Contains(err.Error(), "private-marker") {
			t.Fatal("invalid input leaked into error")
		}
	}
	negative := testProjectQuotaMutation()
	negative.Quota.Spec.Hard = testProjectResources("cpu", "-1")
	if validateProjectQuotaMutation(negative) == nil {
		t.Fatal("negative quota accepted")
	}
	unknownScope := testProjectQuotaMutation()
	unknownScope.Quota.Spec.Scopes = []corev1.ResourceQuotaScope{"Imaginary"}
	if validateProjectQuotaMutation(unknownScope) == nil {
		t.Fatal("unknown quota scope accepted")
	}
	update := testProjectQuotaMutation()
	update.Action = "quota_update"
	if validateProjectQuotaMutation(update) == nil {
		t.Fatal("update without preconditions accepted")
	}
	update.Quota.UID, update.Quota.ResourceVersion = "quota-1", "7"
	if err := validateProjectQuotaMutation(update); err != nil {
		t.Fatal(err)
	}
	update.Action = "quota_create"
	if validateProjectQuotaMutation(update) == nil {
		t.Fatal("create with update preconditions accepted")
	}
}

func TestResourceLimitRangeRejectsInconsistentBoundsAndUnsupportedDefaults(t *testing.T) {
	valid := corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Min: testProjectResources("cpu", "100m"), Max: testProjectResources("cpu", "2"), Default: testProjectResources("cpu", "1"), DefaultRequest: testProjectResources("cpu", "500m")}
	if err := validateProjectLimitSpec(corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{valid}}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []corev1.LimitRangeItem{
		{Type: "Imaginary", Max: testProjectResources("cpu", "1")},
		{Type: corev1.LimitTypeContainer},
		{Type: corev1.LimitTypeContainer, Min: testProjectResources("cpu", "2"), Max: testProjectResources("cpu", "1")},
		{Type: corev1.LimitTypeContainer, Default: testProjectResources("cpu", "1"), DefaultRequest: testProjectResources("cpu", "2")},
		{Type: corev1.LimitTypeContainer, MaxLimitRequestRatio: testProjectResources("cpu", "0.5")},
		{Type: corev1.LimitTypePod, Default: testProjectResources("cpu", "1")},
		{Type: corev1.LimitTypePersistentVolumeClaim, Max: testProjectResources("cpu", "1")},
		{Type: corev1.LimitTypeContainer, Min: testProjectResources("cpu", "-1")},
	} {
		if validateProjectLimitSpec(corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{item}}) == nil {
			t.Fatalf("invalid limit accepted: %+v", item)
		}
	}
}

func TestResourceQuotaUpdateUsesPreconditionsAndPreservesMetadata(t *testing.T) {
	current := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "budget", Namespace: "apps", UID: "quota-1", ResourceVersion: "7", Labels: map[string]string{"owner": "web"}, Annotations: map[string]string{"operator": "preserve"}}, Spec: corev1.ResourceQuotaSpec{Hard: testProjectResources("requests.cpu", "1")}}
	cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}}, current)
	mutation := testProjectQuotaMutation()
	mutation.Action = "quota_update"
	mutation.Quota.UID, mutation.Quota.ResourceVersion = current.UID, current.ResourceVersion
	dryRunCount := 0
	cs.PrependReactor("update", "resourcequotas", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update := action.(k8stesting.UpdateActionImpl)
		value := update.GetObject().(*corev1.ResourceQuota)
		if value.UID != current.UID || value.ResourceVersion != current.ResourceVersion || value.Labels["owner"] != "web" || value.Annotations["operator"] != "preserve" {
			t.Fatal("update lost identity preconditions or unrelated metadata")
		}
		if len(update.GetUpdateOptions().DryRun) != 0 {
			dryRunCount++
			return true, value.DeepCopy(), nil
		}
		return false, nil, nil
	})
	if _, err := executeProjectQuotaMutation(context.Background(), &kubeClients{cs: cs}, mutation, true); err != nil {
		t.Fatal(err)
	}
	if dryRunCount != 1 {
		t.Fatal("preflight was not a Kubernetes dry-run")
	}
	read, _ := cs.CoreV1().ResourceQuotas("apps").Get(context.Background(), "budget", metav1.GetOptions{})
	quantity := read.Spec.Hard[corev1.ResourceRequestsCPU]
	if quantity.String() != "1" {
		t.Fatal("dry-run persisted a mutation")
	}
	if _, err := executeProjectQuotaMutation(context.Background(), &kubeClients{cs: cs}, mutation, false); err != nil {
		t.Fatal(err)
	}
	mutation.Quota.ResourceVersion = "stale"
	before := len(cs.Actions())
	_, err := executeProjectQuotaMutation(context.Background(), &kubeClients{cs: cs}, mutation, false)
	var platform *httpx.Error
	if !errors.As(err, &platform) || platform.Status != http.StatusConflict {
		t.Fatalf("stale update not rejected: %v", err)
	}
	for _, action := range cs.Actions()[before:] {
		if action.GetVerb() == "update" {
			t.Fatal("stale approval attempted a write")
		}
	}
}

type testProjectApprovalRepo struct{ tickets []*approval.Ticket }

func (r *testProjectApprovalRepo) Insert(_ context.Context, ticket *approval.Ticket) error {
	ticket.ID = int64(len(r.tickets) + 1)
	r.tickets = append(r.tickets, ticket)
	return nil
}
func (*testProjectApprovalRepo) Get(context.Context, int64) (*approval.Ticket, error) {
	return nil, errors.New("unused")
}
func (*testProjectApprovalRepo) Decide(context.Context, int64, string, int64, string, string) (*approval.Ticket, error) {
	return nil, errors.New("unused")
}
func (*testProjectApprovalRepo) Finish(context.Context, int64, string, json.RawMessage, string) error {
	return errors.New("unused")
}
func (*testProjectApprovalRepo) List(context.Context, approval.ListFilter, httpx.Page) ([]approval.Ticket, int64, error) {
	return nil, 0, errors.New("unused")
}

type testProjectApprovalPolicy struct {
	required bool
	err      error
}

func (p testProjectApprovalPolicy) GetEnvPolicy(context.Context, string) (*rbac.EnvPolicy, error) {
	return &rbac.EnvPolicy{RequireApproval: p.required}, p.err
}

func TestResourceQuotaApprovalPendingNeverPersistsResource(t *testing.T) {
	repo := &testProjectApprovalRepo{}
	m := New()
	m.d = &app.Deps{Approval: approval.New(repo, testProjectApprovalPolicy{required: true}, nil)}
	if err := m.RegisterProjectQuotaApprovals(); err != nil {
		t.Fatal(err)
	}
	cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}})
	dryRunCount := 0
	cs.PrependReactor("create", "resourcequotas", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateActionImpl)
		if len(create.GetCreateOptions().DryRun) != 1 || create.GetCreateOptions().DryRun[0] != metav1.DryRunAll {
			t.Fatal("pending approval performed a real write")
		}
		dryRunCount++
		return true, create.GetObject().DeepCopyObject(), nil
	})
	p := &rbac.Principal{UserID: 1, Username: "writer", Permissions: []string{PermNSWrite}}
	_, ticket, err := m.runProjectQuotaMutation(context.Background(), p, &Cluster{ID: 1, Name: "test", Env: "prod"}, &kubeClients{cs: cs}, testProjectQuotaMutation(), "capacity")
	if err != nil || ticket == nil || len(repo.tickets) != 1 || dryRunCount != 1 {
		t.Fatalf("gated change failed: %v %v", ticket, err)
	}
	if _, err := cs.CoreV1().ResourceQuotas("apps").Get(context.Background(), "budget", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("pending approval persisted a quota")
	}
}

func TestResourceQuotaMissingApprovalOrPermissionFailsClosed(t *testing.T) {
	m := New()
	p := &rbac.Principal{UserID: 1, Permissions: []string{PermNSWrite}}
	cl := &Cluster{ID: 1, Name: "test", Env: "test"}
	mutation := testProjectQuotaMutation()
	if _, _, err := m.runProjectQuotaMutation(context.Background(), p, cl, nil, mutation, ""); err == nil {
		t.Fatal("missing approval service allowed mutation")
	}
	m.d = &app.Deps{Approval: approval.New(&testProjectApprovalRepo{}, testProjectApprovalPolicy{}, nil)}
	if _, _, err := m.runProjectQuotaMutation(context.Background(), p, cl, nil, mutation, ""); err == nil {
		t.Fatal("unregistered approval handler allowed mutation")
	}
	if err := m.RegisterProjectQuotaApprovals(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.runProjectQuotaMutation(context.Background(), &rbac.Principal{}, cl, nil, mutation, ""); err == nil {
		t.Fatal("missing namespace permission allowed mutation")
	}
	if _, _, err := m.runProjectQuotaMutation(context.Background(), &rbac.Principal{Permissions: []string{PermNSWrite}, EnvScope: []string{"other"}}, cl, nil, mutation, ""); err == nil {
		t.Fatal("wrong environment allowed mutation")
	}
	if _, err := m.onProjectQuotaApproved(context.Background(), &approval.Ticket{Type: projectQuotaApprovalType, Payload: []byte(`{}`)}); err == nil {
		t.Fatal("approval callback missing RBAC service allowed mutation")
	}
}

func TestResourceQuotaAdmissionDenialDoesNotCreateApprovalOrWrite(t *testing.T) {
	repo := &testProjectApprovalRepo{}
	m := New()
	m.d = &app.Deps{Approval: approval.New(repo, testProjectApprovalPolicy{required: true}, nil)}
	if err := m.RegisterProjectQuotaApprovals(); err != nil {
		t.Fatal(err)
	}
	cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}})
	cs.PrependReactor("create", "resourcequotas", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if len(action.(k8stesting.CreateActionImpl).GetCreateOptions().DryRun) == 0 {
			t.Fatal("dry-run denial fell back to a real write")
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "resourcequotas"}, "budget", errors.New("private-marker"))
	})
	_, _, err := m.runProjectQuotaMutation(context.Background(), &rbac.Principal{UserID: 1, Permissions: []string{PermNSWrite}}, &Cluster{ID: 1, Name: "test", Env: "prod"}, &kubeClients{cs: cs}, testProjectQuotaMutation(), "")
	if err == nil || len(repo.tickets) != 0 || strings.Contains(err.Error(), "private-marker") {
		t.Fatalf("denial was bypassed or leaked: %v", err)
	}
}

func TestResourceNamespacePreflightIsDryRunAndNeverFallsBack(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmtBool(denied), func(t *testing.T) {
			cs := kubeFake.NewSimpleClientset()
			calls := 0
			cs.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				calls++
				create := action.(k8stesting.CreateActionImpl)
				options := create.GetCreateOptions()
				if len(options.DryRun) != 1 || options.DryRun[0] != metav1.DryRunAll || options.FieldManager != fieldManager {
					t.Fatal("namespace preflight attempted a real or unowned write")
				}
				value := create.GetObject().(*corev1.Namespace)
				if value.Name != "apps" || value.Labels["team"] != "web" {
					t.Fatal("namespace preflight changed the submitted configuration")
				}
				if denied {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "apps", errors.New("private-marker"))
				}
				return true, value.DeepCopy(), nil
			})
			err := preflightProjectNamespace(context.Background(), &kubeClients{cs: cs}, nsParams{Name: "apps", Labels: map[string]string{"team": "web"}})
			if (err != nil) != denied || calls != 1 || (err != nil && strings.Contains(err.Error(), "private-marker")) {
				t.Fatalf("namespace preflight bypassed denial or leaked error: %v, calls=%d", err, calls)
			}
			if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "apps", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("namespace preflight persisted a namespace")
			}
		})
	}
}

func fmtBool(value bool) string {
	if value {
		return "denied"
	}
	return "accepted"
}

func TestResourceQuotaDirectExecutionRequiresDryRunAndReadablePolicy(t *testing.T) {
	for _, policyFails := range []bool{false, true} {
		t.Run(fmtBool(policyFails), func(t *testing.T) {
			repo := &testProjectApprovalRepo{}
			policy := testProjectApprovalPolicy{}
			if policyFails {
				policy.err = errors.New("policy unavailable")
			}
			m := New()
			m.d = &app.Deps{Approval: approval.New(repo, policy, nil)}
			if err := m.RegisterProjectQuotaApprovals(); err != nil {
				t.Fatal(err)
			}
			cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}})
			var dryRuns, writes int
			cs.PrependReactor("create", "resourcequotas", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create := action.(k8stesting.CreateActionImpl)
				if len(create.GetCreateOptions().DryRun) != 0 {
					dryRuns++
					return true, create.GetObject().DeepCopyObject(), nil
				}
				if dryRuns != 1 || policyFails {
					t.Fatal("real write preceded dry-run or bypassed unreadable policy")
				}
				writes++
				return false, nil, nil
			})
			_, ticket, err := m.runProjectQuotaMutation(context.Background(),
				&rbac.Principal{UserID: 1, Permissions: []string{PermNSWrite}},
				&Cluster{ID: 1, Name: "test", Env: "test"}, &kubeClients{cs: cs}, testProjectQuotaMutation(), "")
			if (err != nil) != policyFails || ticket != nil || len(repo.tickets) != 0 || dryRuns != 1 {
				t.Fatalf("incorrect direct change outcome: %v %v; dry-runs=%d", ticket, err, dryRuns)
			}
			wantWrites := 1
			if policyFails {
				wantWrites = 0
			}
			if writes != wantWrites {
				t.Fatalf("real writes=%d, want %d", writes, wantWrites)
			}
		})
	}
}

func TestResourceQuotaEnvironmentDenialMakesNoKubernetesCalls(t *testing.T) {
	m := New()
	m.d = &app.Deps{Approval: approval.New(&testProjectApprovalRepo{}, testProjectApprovalPolicy{}, nil)}
	if err := m.RegisterProjectQuotaApprovals(); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"", " ", "prod"} {
		cs := kubeFake.NewSimpleClientset()
		_, _, err := m.runProjectQuotaMutation(context.Background(),
			&rbac.Principal{UserID: 1, Permissions: []string{PermNSWrite}, EnvScope: []string{"test"}},
			&Cluster{ID: 1, Name: "test", Env: env}, &kubeClients{cs: cs}, testProjectQuotaMutation(), "")
		if err == nil || len(cs.Actions()) != 0 {
			t.Fatalf("environment %q allowed Kubernetes access: %v", env, err)
		}
	}
}

func TestResourceLimitRangeExecutionPreservesMetadataAndRejectsStaleIdentity(t *testing.T) {
	current := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{
		Name: "defaults", Namespace: "apps", UID: "limit-1", ResourceVersion: "8",
		Labels: map[string]string{"owner": "web"}, Annotations: map[string]string{"operator": "preserve"},
	}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer, Default: testProjectResources("cpu", "1")}}}}
	cs := kubeFake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}}, current)
	mutation := projectQuotaMutation{ClusterID: 1, Cluster: "test", Action: "limitrange_update", LimitRange: &projectLimitParams{
		Namespace: "apps", Name: "defaults", UID: current.UID, ResourceVersion: current.ResourceVersion,
		Spec: &corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer, Default: testProjectResources("cpu", "2")}}},
	}}
	dryRuns, writes := 0, 0
	cs.PrependReactor("update", "limitranges", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update := action.(k8stesting.UpdateActionImpl)
		value := update.GetObject().(*corev1.LimitRange)
		if value.UID != current.UID || value.ResourceVersion != current.ResourceVersion || value.Labels["owner"] != "web" ||
			value.Annotations["operator"] != "preserve" || value.Spec.Limits[0].Default.Cpu().String() != "2" {
			t.Fatal("LimitRange update lost metadata or changed the approved spec")
		}
		if len(update.GetUpdateOptions().DryRun) != 0 {
			dryRuns++
			return true, value.DeepCopy(), nil
		}
		writes++
		return false, nil, nil
	})
	for _, dryRun := range []bool{true, false} {
		if _, err := executeProjectQuotaMutation(context.Background(), &kubeClients{cs: cs}, mutation, dryRun); err != nil {
			t.Fatal(err)
		}
	}
	mutation.LimitRange.UID = "replaced"
	if _, err := executeProjectQuotaMutation(context.Background(), &kubeClients{cs: cs}, mutation, false); err == nil || dryRuns != 1 || writes != 1 {
		t.Fatalf("stale LimitRange approval was executed: %v; dry-runs=%d writes=%d", err, dryRuns, writes)
	}
}

type testProjectAuditWriter struct{ entries []audit.Entry }

func (w *testProjectAuditWriter) WriteBatch(_ context.Context, entries []audit.Entry) error {
	w.entries = append(w.entries, entries...)
	return nil
}

func TestResourceQuotaApprovedExecutionRecordsSanitizedFailure(t *testing.T) {
	writer := &testProjectAuditWriter{}
	recorder := audit.NewRecorder(writer, nil)
	m := New()
	m.d = &app.Deps{Audit: recorder}
	payload, err := json.Marshal(testProjectQuotaMutation())
	if err != nil {
		t.Fatal(err)
	}
	ticket := &approval.Ticket{ID: 9, Type: projectQuotaApprovalType, Status: approval.StatusApproved,
		ApplicantID: 1, ApplicantName: "writer", Env: "prod", Payload: payload}
	if _, err := m.onProjectQuotaApproved(context.Background(), ticket); err == nil {
		t.Fatal("missing execution services allowed an approved change")
	}
	recorder.Close()
	if len(writer.entries) != 1 || writer.entries[0].Result != audit.ResultFailure ||
		writer.entries[0].Method != "APPROVAL" || writer.entries[0].UserID == nil || *writer.entries[0].UserID != 1 ||
		writer.entries[0].StatusCode != http.StatusServiceUnavailable || writer.entries[0].ResourceID != "apps/budget" {
		t.Fatalf("approved execution failure was not audited: %+v", writer.entries)
	}
	raw, _ := json.Marshal(writer.entries)
	if strings.Contains(string(raw), "requests.cpu") || strings.Contains(string(raw), "spec") {
		t.Fatal("resource configuration escaped the execution audit")
	}
}

func TestResourceQuotaExecutionAuditHasSuccessIdentityWithoutConfiguration(t *testing.T) {
	writer := &testProjectAuditWriter{}
	recorder := audit.NewRecorder(writer, nil)
	m := New()
	m.d = &app.Deps{Audit: recorder}
	m.auditProjectQuotaExecution(context.Background(), &approval.Ticket{ID: 9, ApplicantID: 1, ApplicantName: "writer", Env: "prod"},
		testProjectQuotaMutation(), time.Now(), nil)
	recorder.Close()
	if len(writer.entries) != 1 || writer.entries[0].Result != audit.ResultSuccess ||
		writer.entries[0].ResourceID != "apps/budget" || writer.entries[0].StatusCode != http.StatusOK {
		t.Fatalf("incorrect success execution audit: %+v", writer.entries)
	}
	var detail map[string]any
	if err := json.Unmarshal(writer.entries[0].Detail, &detail); err != nil || detail["ticket_id"] != float64(9) || detail["cluster_id"] != float64(1) {
		t.Fatalf("execution audit missing correlation identity: %+v %v", detail, err)
	}
}
