// Package approval implements generic approval tickets. Modules register a
// ticket type with a Handler; when a ticket is approved the handler executes the
// gated action (e.g. a production deploy) and the outcome is written back.
//
// Typical module flow:
//
//	t, pending, err := d.Approval.Gate(ctx, principal, approval.CreateInput{
//	    Type: "cicd.deploy", Env: "prod", Title: "发布 order-service v1.4.2", Payload: req})
//	if err != nil { httpx.Fail(c, err); return }
//	if pending { approval.RespondPending(c, t); return }
//	// not gated: execute immediately
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Ticket statuses.
const (
	StatusPending   = "pending"
	StatusApproved  = "approved" // approved, handler running
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
	StatusExecuted  = "executed" // handler succeeded
	StatusFailed    = "failed"   // handler returned an error
)

// Ticket is an approval request.
type Ticket struct {
	ID            int64           `json:"id"`
	Type          string          `json:"type"`
	Title         string          `json:"title"`
	Env           string          `json:"env"`
	Reason        string          `json:"reason"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	ApplicantID   int64           `json:"applicant_id"`
	ApplicantName string          `json:"applicant_name"`
	ApproverID    *int64          `json:"approver_id"`
	ApproverName  string          `json:"approver_name"`
	Comment       string          `json:"comment"`
	Result        json.RawMessage `json:"result"`
	Error         string          `json:"error"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	DecidedAt     *time.Time      `json:"decided_at"`
	ExecutedAt    *time.Time      `json:"executed_at"`
}

// DecodePayload unmarshals the payload into v.
func (t *Ticket) DecodePayload(v any) error { return json.Unmarshal(t.Payload, v) }

// Handler executes the gated action of a ticket type.
type Handler interface {
	// OnApproved runs after approval with ctx carrying the applicant as
	// principal (rbac.FromContext). The returned value is stored as the
	// ticket result; an error marks the ticket failed.
	OnApproved(ctx context.Context, t *Ticket) (result any, err error)
}

// RejectHandler is optionally implemented by a Handler to react to rejection/cancel.
type RejectHandler interface {
	OnRejected(ctx context.Context, t *Ticket) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, t *Ticket) (any, error)

func (f HandlerFunc) OnApproved(ctx context.Context, t *Ticket) (any, error) { return f(ctx, t) }

// TypeSpec registers a ticket type.
type TypeSpec struct {
	Type        string `json:"type"`        // e.g. "cicd.deploy"
	Module      string `json:"module"`      // owning module
	Description string `json:"description"` // shown in the UI
	// CreatePermission is required to open a ticket of this type directly via
	// POST /approvals (defaults to approval:create). Gate() skips it because the
	// calling module has already authorised the action.
	CreatePermission string `json:"create_permission"`
	// ApproverPermission overrides the env policy's approver permission.
	ApproverPermission string        `json:"approver_permission"`
	Timeout            time.Duration `json:"-"` // handler timeout, default 10m
	Handler            Handler       `json:"-"`
}

// PolicySource provides environment change policies (rbac.Service).
type PolicySource interface {
	GetEnvPolicy(ctx context.Context, env string) (*rbac.EnvPolicy, error)
}

// Service manages tickets.
type Service struct {
	repo   Repo
	policy PolicySource
	log    *slog.Logger

	mu    sync.RWMutex
	types map[string]TypeSpec
	wg    sync.WaitGroup
}

// New builds the service and registers the built-in "generic" type (no action).
func New(repo Repo, policy PolicySource, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{repo: repo, policy: policy, log: log, types: map[string]TypeSpec{}}
	s.MustRegister(TypeSpec{Type: "generic", Module: "core", Description: "通用审批（无自动执行动作）",
		Handler: HandlerFunc(func(context.Context, *Ticket) (any, error) { return nil, nil })})
	return s
}

// Register adds a ticket type.
func (s *Service) Register(spec TypeSpec) error {
	if spec.Type == "" || spec.Handler == nil {
		return errors.New("approval: type and handler are required")
	}
	if spec.CreatePermission == "" {
		spec.CreatePermission = "approval:create"
	}
	if spec.Timeout <= 0 {
		spec.Timeout = 10 * time.Minute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.types[spec.Type]; dup {
		return fmt.Errorf("approval: type %q already registered", spec.Type)
	}
	s.types[spec.Type] = spec
	return nil
}

// MustRegister panics on error.
func (s *Service) MustRegister(spec TypeSpec) {
	if err := s.Register(spec); err != nil {
		panic(err)
	}
}

// Types lists registered ticket types.
func (s *Service) Types() []TypeSpec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TypeSpec, 0, len(s.types))
	for _, t := range s.types {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

func (s *Service) spec(typ string) (TypeSpec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.types[typ]
	return t, ok
}

// CreateInput opens a ticket.
type CreateInput struct {
	Type    string `json:"type" binding:"required,max=64"`
	Title   string `json:"title" binding:"required,max=256"`
	Env     string `json:"env" binding:"max=64"`
	Reason  string `json:"reason" binding:"max=2000"`
	Payload any    `json:"payload"`
}

// Create opens a ticket of a registered type. checkPerm enforces the type's CreatePermission.
func (s *Service) create(ctx context.Context, p *rbac.Principal, in CreateInput, checkPerm bool) (*Ticket, error) {
	spec, ok := s.spec(in.Type)
	if !ok {
		return nil, httpx.Validation("unknown approval type: " + in.Type)
	}
	if checkPerm && !p.Can(spec.CreatePermission) {
		return nil, httpx.Forbidden("permission denied: " + spec.CreatePermission)
	}
	if in.Env != "" && !p.CanEnv(in.Env) {
		return nil, httpx.Forbidden("environment not in your scope: " + in.Env)
	}
	payload := json.RawMessage("{}")
	if in.Payload != nil {
		b, err := json.Marshal(in.Payload)
		if err != nil {
			return nil, httpx.Validation("payload is not serialisable")
		}
		payload = b
	}
	t := &Ticket{Type: in.Type, Title: in.Title, Env: in.Env, Reason: in.Reason, Payload: payload,
		Status: StatusPending, ApplicantID: p.UserID, ApplicantName: p.Username}
	if err := s.repo.Insert(ctx, t); err != nil {
		return nil, err
	}
	s.log.Info("approval ticket created", "id", t.ID, "type", t.Type, "env", t.Env, "applicant", p.Username)
	return t, nil
}

// Create opens a ticket on behalf of p (POST /approvals).
func (s *Service) Create(ctx context.Context, p *rbac.Principal, in CreateInput) (*Ticket, error) {
	return s.create(ctx, p, in, true)
}

// Gate is what modules call before a write operation on an environment. If the
// environment's policy requires approval it opens a ticket and returns
// (ticket, true, nil); the registered handler performs the action once approved.
// Otherwise it returns (nil, false, nil) and the caller proceeds immediately.
func (s *Service) Gate(ctx context.Context, p *rbac.Principal, in CreateInput) (*Ticket, bool, error) {
	if in.Env == "" {
		return nil, false, nil
	}
	if !p.CanEnv(in.Env) {
		return nil, false, httpx.Forbidden("environment not in your scope: " + in.Env)
	}
	pol, err := s.policy.GetEnvPolicy(ctx, in.Env)
	if err != nil {
		return nil, false, err
	}
	if pol == nil || !pol.RequireApproval {
		return nil, false, nil
	}
	t, err := s.create(ctx, p, in, false)
	if err != nil {
		return nil, false, err
	}
	return t, true, nil
}

// RespondPending writes HTTP 202 with code CodeApprovalRequired and the ticket.
func RespondPending(c *gin.Context, t *Ticket) {
	c.JSON(http.StatusAccepted, httpx.Envelope{
		Code: httpx.CodeApprovalRequired, Message: "该环境的变更需要审批，已创建审批单",
		Data: gin.H{"approval_required": true, "ticket": t}, RequestID: httpx.RequestIDFrom(c),
	})
}

// approverPermission resolves which permission may decide on t.
func (s *Service) approverPermission(ctx context.Context, t *Ticket) (string, error) {
	if spec, ok := s.spec(t.Type); ok && spec.ApproverPermission != "" {
		return spec.ApproverPermission, nil
	}
	if t.Env != "" {
		pol, err := s.policy.GetEnvPolicy(ctx, t.Env)
		if err != nil {
			return "", err
		}
		if pol != nil && pol.ApproverPermission != "" {
			return pol.ApproverPermission, nil
		}
	}
	return "approval:approve", nil
}

// CheckDecide returns nil when p may approve/reject t.
func CheckDecide(p *rbac.Principal, t *Ticket, approverPerm string) error {
	if t.Status != StatusPending {
		return httpx.Conflict("ticket is not pending (status: " + t.Status + ")")
	}
	if t.ApplicantID == p.UserID {
		return httpx.Forbidden("不能审批自己发起的审批单")
	}
	if !p.Can(approverPerm) {
		return httpx.Forbidden("permission denied: " + approverPerm)
	}
	if t.Env != "" && !p.CanEnv(t.Env) {
		return httpx.Forbidden("environment not in your scope: " + t.Env)
	}
	return nil
}

// Approve approves a pending ticket and runs its handler asynchronously.
func (s *Service) Approve(ctx context.Context, p *rbac.Principal, id int64, comment string) (*Ticket, error) {
	t, err := s.decide(ctx, p, id, StatusApproved, comment)
	if err != nil {
		return nil, err
	}
	spec, ok := s.spec(t.Type)
	if !ok {
		_ = s.repo.Finish(context.WithoutCancel(ctx), t.ID, StatusFailed, nil, "approval type no longer registered")
		t.Status, t.Error = StatusFailed, "approval type no longer registered"
		return t, nil
	}
	s.wg.Add(1)
	go s.execute(spec, *t)
	return t, nil
}

func (s *Service) execute(spec TypeSpec, t Ticket) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), spec.Timeout)
	defer cancel()
	ctx = rbac.WithPrincipal(ctx, &rbac.Principal{UserID: t.ApplicantID, Username: t.ApplicantName})
	var result any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("handler panic: %v", r)
			}
		}()
		result, err = spec.Handler.OnApproved(ctx, &t)
	}()
	var resJSON json.RawMessage
	if result != nil {
		if b, mErr := json.Marshal(result); mErr == nil {
			resJSON = b
		}
	}
	status, msg := StatusExecuted, ""
	if err != nil {
		status, msg = StatusFailed, err.Error()
		s.log.Error("approval handler failed", "id", t.ID, "type", t.Type, "error", err)
	} else {
		s.log.Info("approval handler executed", "id", t.ID, "type", t.Type)
	}
	if fErr := s.repo.Finish(context.Background(), t.ID, status, resJSON, msg); fErr != nil {
		s.log.Error("approval finish write failed", "id", t.ID, "error", fErr)
	}
}

// Reject rejects a pending ticket.
func (s *Service) Reject(ctx context.Context, p *rbac.Principal, id int64, comment string) (*Ticket, error) {
	t, err := s.decide(ctx, p, id, StatusRejected, comment)
	if err != nil {
		return nil, err
	}
	s.notifyRejected(t)
	return t, nil
}

func (s *Service) decide(ctx context.Context, p *rbac.Principal, id int64, status, comment string) (*Ticket, error) {
	cur, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	perm, err := s.approverPermission(ctx, cur)
	if err != nil {
		return nil, err
	}
	if err := CheckDecide(p, cur, perm); err != nil {
		return nil, err
	}
	t, err := s.repo.Decide(ctx, id, status, p.UserID, p.Username, comment)
	if errors.Is(err, errNotPending) {
		return nil, httpx.Conflict("ticket was decided concurrently")
	}
	if err != nil {
		return nil, err
	}
	s.log.Info("approval ticket decided", "id", id, "status", status, "approver", p.Username)
	return t, nil
}

// Cancel withdraws a pending ticket (applicant only).
func (s *Service) Cancel(ctx context.Context, p *rbac.Principal, id int64) (*Ticket, error) {
	cur, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.ApplicantID != p.UserID {
		return nil, httpx.Forbidden("只有申请人可以撤回审批单")
	}
	if cur.Status != StatusPending {
		return nil, httpx.Conflict("ticket is not pending (status: " + cur.Status + ")")
	}
	t, err := s.repo.Decide(ctx, id, StatusCancelled, p.UserID, p.Username, "")
	if errors.Is(err, errNotPending) {
		return nil, httpx.Conflict("ticket was decided concurrently")
	}
	if err != nil {
		return nil, err
	}
	s.notifyRejected(t)
	return t, nil
}

func (s *Service) notifyRejected(t *Ticket) {
	spec, ok := s.spec(t.Type)
	if !ok {
		return
	}
	if rh, ok := spec.Handler.(RejectHandler); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := rh.OnRejected(ctx, t); err != nil {
			s.log.Error("approval reject hook failed", "id", t.ID, "type", t.Type, "error", err)
		}
	}
}

// Get returns a ticket visible to p (applicant, approval:read, or eligible approver).
func (s *Service) Get(ctx context.Context, p *rbac.Principal, id int64) (*Ticket, error) {
	t, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.ApplicantID == p.UserID || p.Can("approval:read") {
		return t, nil
	}
	perm, err := s.approverPermission(ctx, t)
	if err != nil {
		return nil, err
	}
	if p.Can(perm) {
		return t, nil
	}
	return nil, httpx.ErrNotFound
}

// List returns tickets. scope: "mine" (applicant = p), "todo" (pending and p can
// approve), "all" (requires approval:read).
func (s *Service) List(ctx context.Context, p *rbac.Principal, scope string, f ListFilter, pg httpx.Page) ([]Ticket, int64, error) {
	switch scope {
	case "", "mine":
		f.ApplicantID = p.UserID
	case "todo":
		f.Status = StatusPending
		f.ExcludeApplicantID = p.UserID
		items, _, err := s.repo.List(ctx, f, httpx.Page{Page: 1, Size: 1000})
		if err != nil {
			return nil, 0, err
		}
		var out []Ticket
		for i := range items {
			perm, err := s.approverPermission(ctx, &items[i])
			if err != nil {
				return nil, 0, err
			}
			if p.Can(perm) && (items[i].Env == "" || p.CanEnv(items[i].Env)) {
				out = append(out, items[i])
			}
		}
		total := int64(len(out))
		lo, hi := pg.Offset(), pg.Offset()+pg.Limit()
		if lo > len(out) {
			lo = len(out)
		}
		if hi > len(out) {
			hi = len(out)
		}
		return out[lo:hi], total, nil
	case "all":
		if !p.Can("approval:read") {
			return nil, 0, httpx.Forbidden("permission denied: approval:read")
		}
	default:
		return nil, 0, httpx.Validation("scope must be mine, todo or all")
	}
	return s.repo.List(ctx, f, pg)
}

// Wait blocks until running handlers finish (graceful shutdown).
func (s *Service) Wait() { s.wg.Wait() }
