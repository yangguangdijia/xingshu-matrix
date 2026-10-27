package approval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

type nilApprovalHandler struct{}

func (*nilApprovalHandler) OnApproved(context.Context, *Ticket) (any, error) { return nil, nil }

type gateRepo struct {
	inserted []Ticket
	fail     error
}

func (r *gateRepo) Insert(_ context.Context, t *Ticket) error {
	if r.fail != nil {
		return r.fail
	}
	t.ID = int64(len(r.inserted) + 1)
	r.inserted = append(r.inserted, *t)
	return nil
}
func (r *gateRepo) Get(_ context.Context, id int64) (*Ticket, error) {
	if id <= 0 || int(id) > len(r.inserted) {
		return nil, httpx.ErrNotFound
	}
	t := r.inserted[id-1]
	return &t, nil
}
func (r *gateRepo) Decide(_ context.Context, id int64, status string, approverID int64, approverName, comment string) (*Ticket, error) {
	if id <= 0 || int(id) > len(r.inserted) || r.inserted[id-1].Status != StatusPending {
		return nil, errNotPending
	}
	t := &r.inserted[id-1]
	t.Status, t.ApproverID, t.ApproverName, t.Comment = status, &approverID, approverName, comment
	return r.Get(context.Background(), id)
}
func (r *gateRepo) Finish(context.Context, int64, string, json.RawMessage, string) error {
	return nil
}
func (r *gateRepo) List(_ context.Context, f ListFilter, _ httpx.Page) ([]Ticket, int64, error) {
	var items []Ticket
	for _, ticket := range r.inserted {
		if f.ApplicantID > 0 && ticket.ApplicantID != f.ApplicantID {
			continue
		}
		if f.ExcludeApplicantID > 0 && ticket.ApplicantID == f.ExcludeApplicantID {
			continue
		}
		if f.Status != "" && ticket.Status != f.Status {
			continue
		}
		items = append(items, ticket)
	}
	return items, int64(len(items)), nil
}

type gatePolicies struct {
	byEnv map[string]*rbac.EnvPolicy
	fail  error
}

func (p *gatePolicies) GetEnvPolicy(_ context.Context, env string) (*rbac.EnvPolicy, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	return p.byEnv[env], nil
}

func gatePrincipal(scope ...string) *rbac.Principal {
	return &rbac.Principal{UserID: 7, Username: "alice", Permissions: []string{"workload:write"}, EnvScope: scope}
}

func TestIsProductionEnv(t *testing.T) {
	for _, env := range []string{"prod", "Production", " PRD ", "live", "prod-east", "production_eu", "production-x", "east-prod", "prod2", "production1-eu", "live2", "生产", "线上", "生产-线上"} {
		if !IsProductionEnv(env) {
			t.Fatalf("%q should be treated as production", env)
		}
	}
	for _, env := range []string{"dev", "test", "staging", "pre", "product", "prodblue", ""} {
		if IsProductionEnv(env) {
			t.Fatalf("%q should not be treated as production", env)
		}
	}
}

func TestGateFailsClosedForProductionVariants(t *testing.T) {
	for _, env := range []string{"prod-east", "east-prod", "prd_eu"} {
		s := New(&gateRepo{}, &gatePolicies{}, nil)
		if _, pending, err := s.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Env: env}); err == nil || pending {
			t.Fatalf("production-like environment %q should reject missing policy: pending=%v err=%v", env, pending, err)
		}
	}
}

func TestGateFailsClosedForProduction(t *testing.T) {
	cases := []struct {
		name     string
		policies *gatePolicies
		repo     *gateRepo
		env      string
	}{
		{name: "missing policy", policies: &gatePolicies{}, repo: &gateRepo{}, env: "prod"},
		{name: "approval disabled", policies: &gatePolicies{byEnv: map[string]*rbac.EnvPolicy{
			"prod": {Env: "prod", RequireApproval: false},
		}}, repo: &gateRepo{}, env: "prod"},
		{name: "policy lookup error", policies: &gatePolicies{fail: errors.New("db down")}, repo: &gateRepo{}, env: "production"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.repo, tc.policies, nil)
			if _, pending, err := s.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Title: "x", Env: tc.env}); err == nil || pending {
				t.Fatalf("expected fail-closed error, pending=%v err=%v", pending, err)
			}
			if len(tc.repo.inserted) != 0 {
				t.Fatalf("no ticket should be created, got %d", len(tc.repo.inserted))
			}
		})
	}
}

func TestGateRequiresPrincipalAndEnvironment(t *testing.T) {
	s := New(&gateRepo{}, &gatePolicies{}, nil)
	if _, _, err := s.Gate(context.Background(), nil, CreateInput{Type: "generic", Env: "prod"}); err == nil {
		t.Fatal("nil principal must be rejected")
	}
	if _, _, err := s.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Env: " "}); err == nil {
		t.Fatal("empty environment must be rejected")
	}
	if _, _, err := s.Gate(context.Background(), gatePrincipal("dev"), CreateInput{Type: "generic", Env: "prod"}); err == nil {
		t.Fatal("environment outside scope must be rejected")
	}
}

func TestGateNonProductionWithoutPolicyProceeds(t *testing.T) {
	for _, env := range []string{"dev", "test", "staging", "dev-east", "qa2"} {
		repo := &gateRepo{}
		s := New(repo, &gatePolicies{}, nil)
		ticket, pending, err := s.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Title: "nonprod change", Env: env})
		if err != nil || pending || ticket != nil || len(repo.inserted) != 0 {
			t.Fatalf("%s without policy: ticket=%v pending=%v err=%v inserted=%d", env, ticket, pending, err, len(repo.inserted))
		}
	}
}

func TestGateUnknownEnvironmentRequiresExplicitApprovalPolicy(t *testing.T) {
	for _, env := range []string{"prodblue", "custom", "preview"} {
		t.Run(env, func(t *testing.T) {
			repo := &gateRepo{}
			s := New(repo, &gatePolicies{}, nil)
			if _, pending, err := s.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Title: "custom change", Env: env}); err == nil || pending {
				t.Fatalf("unknown environment without policy must reject: pending=%v err=%v", pending, err)
			}
			if len(repo.inserted) != 0 {
				t.Fatal("unknown environment must not create a ticket without policy")
			}
		})
	}

	repo := &gateRepo{}
	s := New(repo, &gatePolicies{byEnv: map[string]*rbac.EnvPolicy{
		"prodblue": {Env: "prodblue", RequireApproval: false},
	}}, nil)
	if _, pending, err := s.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Title: "custom change", Env: "prodblue"}); err == nil || pending {
		t.Fatalf("custom env policy with approval disabled must reject: pending=%v err=%v", pending, err)
	}
}

func TestGateProductionWithApprovalCreatesTicket(t *testing.T) {
	repo := &gateRepo{}
	s := New(repo, &gatePolicies{byEnv: map[string]*rbac.EnvPolicy{
		"prod": {Env: "prod", RequireApproval: true, ApproverPermission: "approval:approve"},
	}}, nil)
	ticket, pending, err := s.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Title: "prod change", Env: "prod"})
	if err != nil || !pending || ticket == nil || ticket.ID == 0 {
		t.Fatalf("expected pending ticket: ticket=%v pending=%v err=%v", ticket, pending, err)
	}
	if len(repo.inserted) != 1 || repo.inserted[0].Env != "prod" {
		t.Fatalf("ticket not persisted: %+v", repo.inserted)
	}
}

func TestApprovalPayloadIsSanitizedOutsidePersistence(t *testing.T) {
	repo := &gateRepo{}
	service := New(repo, &gatePolicies{byEnv: map[string]*rbac.EnvPolicy{
		"prod": {Env: "prod", RequireApproval: true, ApproverPermission: "approval:approve"},
	}}, nil)
	var handlerPayload string
	if err := service.Register(TypeSpec{
		Type: "secret.change",
		Handler: HandlerFunc(func(_ context.Context, ticket *Ticket) (any, error) {
			handlerPayload = string(ticket.Payload)
			return nil, nil
		}),
		SanitizePayload: func(json.RawMessage) json.RawMessage {
			return json.RawMessage(`{"params_redacted":true}`)
		},
	}); err != nil {
		t.Fatal(err)
	}

	raw := `{"secret":"do-not-return"}`
	ticket, pending, err := service.Gate(context.Background(), gatePrincipal("*"), CreateInput{
		Type: "secret.change", Title: "secret update", Env: "prod", Payload: json.RawMessage(raw),
	})
	if err != nil || !pending {
		t.Fatalf("Gate() pending=%v err=%v", pending, err)
	}
	if string(ticket.Payload) != `{"params_redacted":true}` {
		t.Fatalf("gate response was not sanitized: %s", ticket.Payload)
	}
	if len(repo.inserted) != 1 || string(repo.inserted[0].Payload) != raw {
		t.Fatalf("stored approval payload unexpectedly changed: %+v", repo.inserted)
	}

	applicant := gatePrincipal("*")
	got, err := service.Get(context.Background(), applicant, ticket.ID)
	if err != nil || string(got.Payload) != `{"params_redacted":true}` {
		t.Fatalf("Get() payload=%s err=%v", got.Payload, err)
	}
	items, _, err := service.List(context.Background(), applicant, "mine", ListFilter{}, httpx.Page{Page: 1, Size: 20})
	if err != nil || len(items) != 1 || string(items[0].Payload) != `{"params_redacted":true}` {
		t.Fatalf("List() items=%+v err=%v", items, err)
	}

	approver := &rbac.Principal{UserID: 8, Username: "bob", Permissions: []string{"approval:approve"}, EnvScope: []string{"*"}}
	approved, err := service.Approve(context.Background(), approver, ticket.ID, "")
	if err != nil || string(approved.Payload) != `{"params_redacted":true}` {
		t.Fatalf("Approve() payload=%s err=%v", approved.Payload, err)
	}
	service.Wait()
	if handlerPayload != raw {
		t.Fatalf("handler received %q, want stored payload %q", handlerPayload, raw)
	}
}

func TestExecutableApprovalCannotBeForgedOrRunAfterPolicyIsDisabled(t *testing.T) {
	repo := &gateRepo{}
	policies := &gatePolicies{byEnv: map[string]*rbac.EnvPolicy{
		"prod": {Env: "prod", RequireApproval: true, ApproverPermission: "approval:approve"},
	}}
	service := New(repo, policies, nil)
	called := false
	if err := service.Register(TypeSpec{Type: "change", Handler: HandlerFunc(func(context.Context, *Ticket) (any, error) {
		called = true
		return nil, nil
	})}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(context.Background(), gatePrincipal("*"), CreateInput{Type: "change", Env: "prod", Payload: map[string]string{"action": "run"}}); err == nil {
		t.Fatal("direct creation of executable approval must be rejected")
	}
	if len(repo.inserted) != 0 {
		t.Fatal("forged approval should not be persisted")
	}
	ticket, pending, err := service.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "change", Env: "prod", Payload: map[string]string{"action": "run"}})
	if err != nil || !pending {
		t.Fatalf("Gate() pending=%v err=%v", pending, err)
	}
	policies.byEnv["prod"].RequireApproval = false
	approver := &rbac.Principal{UserID: 8, Username: "bob", Permissions: []string{"approval:approve"}, EnvScope: []string{"*"}}
	if _, err := service.Approve(context.Background(), approver, ticket.ID, ""); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if called {
		t.Fatal("handler ran after approval policy was disabled")
	}
}

func TestCreateOwnedAllowsPermissionCheckedModuleRequests(t *testing.T) {
	repo := &gateRepo{}
	service := New(repo, nil, nil)
	if err := service.Register(TypeSpec{
		Type: "bastion.access_request", CreatePermission: "bastion:request",
		Handler: HandlerFunc(func(context.Context, *Ticket) (any, error) { return nil, nil }),
	}); err != nil {
		t.Fatal(err)
	}
	input := CreateInput{Type: "bastion.access_request", Env: "prod", Title: "temporary access"}
	principal := &rbac.Principal{UserID: 7, Username: "alice", Permissions: []string{"bastion:request"}, EnvScope: []string{"prod"}}
	if _, err := service.Create(context.Background(), principal, input); err == nil {
		t.Fatal("public Create must not open executable module-owned approvals")
	}
	ticket, err := service.CreateOwned(context.Background(), principal, input)
	if err != nil || ticket == nil || ticket.Type != input.Type {
		t.Fatalf("CreateOwned() ticket=%+v err=%v", ticket, err)
	}
	if _, err := service.CreateOwned(context.Background(), gatePrincipal("*"), input); err == nil {
		t.Fatal("CreateOwned must enforce the module permission")
	}
	outsideScope := &rbac.Principal{UserID: 8, Permissions: []string{"bastion:request"}, EnvScope: []string{"dev"}}
	if _, err := service.CreateOwned(context.Background(), outsideScope, input); err == nil {
		t.Fatal("CreateOwned must enforce environment scope")
	}
}

func TestPolicyIndependentOwnedApprovalExecutesWithoutChangePolicy(t *testing.T) {
	repo := &gateRepo{}
	service := New(repo, nil, nil)
	called := false
	if err := service.Register(TypeSpec{
		Type: "bastion.access_request", CreatePermission: "bastion:request",
		ApproverPermission: "approval:approve", PolicyIndependent: true,
		Handler: HandlerFunc(func(context.Context, *Ticket) (any, error) {
			called = true
			return map[string]bool{"granted": true}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	applicant := &rbac.Principal{UserID: 7, Username: "alice", Permissions: []string{"bastion:request"}, EnvScope: []string{"prod"}}
	ticket, err := service.CreateOwned(context.Background(), applicant, CreateInput{Type: "bastion.access_request", Env: "prod", Title: "temporary access"})
	if err != nil {
		t.Fatal(err)
	}
	approver := &rbac.Principal{UserID: 8, Username: "bob", Permissions: []string{"approval:approve"}, EnvScope: []string{"prod"}}
	if _, err := service.Approve(context.Background(), approver, ticket.ID, "approved"); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if !called {
		t.Fatal("approved policy-independent ticket must execute without the optional environment change policy")
	}
}

func TestGateWithoutApprovalServiceFailsClosed(t *testing.T) {
	var s *Service
	if _, _, err := s.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Env: "dev"}); err == nil {
		t.Fatal("nil service must fail closed")
	}
	unwired := New(nil, nil, nil)
	if _, _, err := unwired.Gate(context.Background(), gatePrincipal("*"), CreateInput{Type: "generic", Env: "dev"}); err == nil {
		t.Fatal("service without repo/policy must fail closed")
	}
}

func TestRegisterRejectsTypedNilHandlers(t *testing.T) {
	service := New(nil, nil, nil)
	var pointer *nilApprovalHandler
	if err := service.Register(TypeSpec{Type: "typed-nil-pointer", Handler: pointer}); err == nil {
		t.Fatal("expected typed nil pointer handler to be rejected")
	}
	var function HandlerFunc
	if err := service.Register(TypeSpec{Type: "typed-nil-function", Handler: function}); err == nil {
		t.Fatal("expected typed nil function handler to be rejected")
	}
	if err := service.Register(TypeSpec{Type: "valid", Handler: &nilApprovalHandler{}}); err != nil {
		t.Fatalf("valid handler rejected: %v", err)
	}
}
