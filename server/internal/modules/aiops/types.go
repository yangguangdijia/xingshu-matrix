// Package aiops is the AI operations workbench: an alert (or an operator's
// request such as "deploy nginx to 10.0.0.5") becomes a Case; the platform
// gathers read-only evidence, the model (or the rule engine when no model is
// configured) writes a conclusion and 1-3 Plans built ONLY from registered
// Actions; an operator reviews a plan and chooses who executes it (AI, human
// checklist, or mixed); every step runs through platform channels (Kubernetes
// API, bastion, component installer) with risk-based approval, verification
// and rollback; a finished case can be saved as a Runbook.
//
// This file is the shared contract. Other files in the package implement it.
package aiops

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Risk levels. read: runs without confirmation. change: needs the reviewer's
// confirmation of the plan. high: additionally needs an approval ticket
// (approval.Gate, a second person) and must be inside the change window when
// one is configured.
const (
	RiskRead   = "read"
	RiskChange = "change"
	RiskHigh   = "high"
)

// Case kinds and sources.
const (
	KindAlert    = "alert"   // started from an alert
	KindDeploy   = "deploy"  // "deploy X to host/cluster"
	KindRequest  = "request" // free-form operator request
	SourceAlert  = "alert"
	SourceChat   = "chat"
	SourceManual = "manual"
	SourceAuto   = "auto" // auto-remediation policy
)

// Case statuses (state machine):
// investigating -> concluded -> (plan_accepted) -> executing -> verifying -> resolved
// any -> failed | cancelled; concluded -> rejected (all plans rejected; may re-investigate)
const (
	CaseInvestigating = "investigating"
	CaseConcluded     = "concluded"
	CaseAccepted      = "plan_accepted"
	CaseAwaitApproval = "awaiting_approval"
	CaseExecuting     = "executing"
	CasePaused        = "paused"
	CaseVerifying     = "verifying"
	CaseResolved      = "resolved"
	CaseFailed        = "failed"
	CaseCancelled     = "cancelled"
	CaseRejected      = "rejected"
)

// Execution modes chosen by the reviewer.
const (
	ModeAI     = "ai"     // platform runs every step
	ModeManual = "manual" // operator runs steps from a checklist and ticks them
	ModeMixed  = "mixed"  // per-step executor (Step.Executor)
)

// Step statuses.
const (
	StepPending    = "pending"
	StepRunning    = "running"
	StepSucceeded  = "succeeded"
	StepFailed     = "failed"
	StepSkipped    = "skipped"
	StepBlocked    = "blocked" // command filter / approval rejected
	StepManual     = "manual_done"
	StepRolledBack = "rolled_back"
)

// Case is one investigation + remediation.
type Case struct {
	ID          int64           `json:"id"`
	Kind        string          `json:"kind"`
	Source      string          `json:"source"`
	Title       string          `json:"title"`
	Status      string          `json:"status"`
	Env         string          `json:"env"`
	ClusterID   *int64          `json:"cluster_id,omitempty"`
	AlertID     *int64          `json:"alert_id,omitempty"`
	AssetID     *int64          `json:"asset_id,omitempty"`
	Request     string          `json:"request"`  // operator's words / alert summary (no secrets)
	Target      json.RawMessage `json:"target"`   // {"kind":"deployment","namespace":"","name":""} or {"asset_id":1}
	Evidence    json.RawMessage `json:"evidence"` // []EvidenceItem
	Conclusion  json.RawMessage `json:"conclusion"`
	Mode        string          `json:"mode,omitempty"`
	PlanID      *int64          `json:"plan_id,omitempty"` // accepted plan
	ApprovalID  *int64          `json:"approval_id,omitempty"`
	RunbookID   *int64          `json:"runbook_id,omitempty"`
	Engine      string          `json:"engine"` // "llm" | "rules"
	Error       string          `json:"error,omitempty"`
	CreatedBy   int64           `json:"created_by"`
	CreatedName string          `json:"created_name"`
	ReviewedBy  string          `json:"reviewed_by,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ResolvedAt  *time.Time      `json:"resolved_at,omitempty"`
}

// EvidenceItem is one read-only observation shown in the UI ("what AI checked").
type EvidenceItem struct {
	Key     string    `json:"key"`    // e.g. "pod_status", "events", "logs", "recent_releases", "host_probe"
	Title   string    `json:"title"`  // Chinese label
	Status  string    `json:"status"` // ok | warn | error | unavailable
	Summary string    `json:"summary"`
	Data    any       `json:"data,omitempty"` // bounded, secrets masked
	At      time.Time `json:"at"`
}

// Finding is produced by a rule analyzer (k8sgpt-style, deterministic).
type Finding struct {
	Analyzer string   `json:"analyzer"` // e.g. "crashloop", "oomkilled", "imagepull", "pending", "node_pressure", "probe_failed", "pvc_pending"
	Severity string   `json:"severity"` // critical | warning | info
	Object   string   `json:"object"`   // "Pod default/web-1"
	Message  string   `json:"message"`  // Chinese, one sentence
	Evidence []string `json:"evidence"`
	// Suggested plans (built from registered actions) the rule engine proposes.
	Plans []PlanDraft `json:"plans,omitempty"`
}

// Conclusion is the AI (or rules) verdict.
type Conclusion struct {
	Summary     string    `json:"summary"`    // one paragraph, Chinese
	RootCause   string    `json:"root_cause"` // or "不确定" with candidates in Hypotheses
	Hypotheses  []string  `json:"hypotheses"`
	Confidence  string    `json:"confidence"` // low | medium | high
	Impact      string    `json:"impact"`
	MissingInfo []string  `json:"missing_info"` // what would raise confidence
	Findings    []Finding `json:"findings,omitempty"`
}

// PlanDraft is a plan before it is stored (from the model or the rule engine).
type PlanDraft struct {
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Steps       []StepDraft `json:"steps"`
	Verify      []StepDraft `json:"verify"` // read actions that confirm recovery
}

// StepDraft references a registered Action by name with JSON params.
type StepDraft struct {
	Action string          `json:"action"`
	Params json.RawMessage `json:"params"`
	Note   string          `json:"note,omitempty"`
}

// Plan is a stored, reviewable plan. Risk = max risk of its steps.
type Plan struct {
	ID          int64     `json:"id"`
	CaseID      int64     `json:"case_id"`
	Seq         int       `json:"seq"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Risk        string    `json:"risk"`
	Impact      string    `json:"impact"`
	EstimateSec int       `json:"estimate_sec"`
	Rollback    string    `json:"rollback"` // human-readable rollback summary
	Status      string    `json:"status"`   // proposed | accepted | rejected | superseded
	RejectNote  string    `json:"reject_note,omitempty"`
	Steps       []Step    `json:"steps"`
	CreatedAt   time.Time `json:"created_at"`
}

// Step is one executable unit of a plan (including verify steps, Phase="verify").
type Step struct {
	ID         int64           `json:"id"`
	PlanID     int64           `json:"plan_id"`
	Seq        int             `json:"seq"`
	Phase      string          `json:"phase"` // main | verify | rollback
	Action     string          `json:"action"`
	Params     json.RawMessage `json:"params"`
	Title      string          `json:"title"` // from Action.Describe
	Risk       string          `json:"risk"`
	Preview    string          `json:"preview"`  // diff / exact command / dry-run output
	Manual     string          `json:"manual"`   // copyable instructions for a human (kubectl / shell)
	Executor   string          `json:"executor"` // ai | human (mixed mode)
	Status     string          `json:"status"`
	Output     string          `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	DoneBy     string          `json:"done_by,omitempty"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// Runbook (预案) is a saved plan template matched to future cases.
type Runbook struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Match       json.RawMessage `json:"match"` // {"alertname":"KubePodCrashLooping","analyzer":"crashloop","env":"prod"}
	Plan        PlanDraft       `json:"plan"`
	Risk        string          `json:"risk"`
	Uses        int             `json:"uses"`
	Successes   int             `json:"successes"`
	AutoExecute bool            `json:"auto_execute"` // whitelist: only allowed when Risk != high
	AutoScope   json.RawMessage `json:"auto_scope"`   // {"cluster_ids":[1],"envs":["test"],"hours":"09-21"}
	SourceCase  *int64          `json:"source_case,omitempty"`
	CreatedBy   string          `json:"created_by"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// ExecContext is handed to Action implementations.
type ExecContext struct {
	Deps      *app.Deps
	Principal *rbac.Principal // the reviewer who accepted the plan (never a superuser stand-in)
	Case      *Case
	Env       string
	ClusterID int64 // 0 when the case has no cluster
	Reason    string
}

// Action is a typed, reviewed operation. Models may only compose plans from
// registered actions; free-form shell exists only as "host.exec" (risk high,
// bastion command filter applies).
type Action interface {
	Name() string // "k8s.restart", "host.exec", ...
	Spec() ActionSpec
	// Validate normalises params and returns the effective risk (may be higher
	// than Spec().Risk, e.g. scale to 0 or production env).
	Validate(ctx context.Context, ec *ExecContext, params json.RawMessage) (json.RawMessage, string, error)
	// Describe returns a Chinese title, a preview (diff / exact command /
	// dry-run) and copyable manual instructions. Must not change state.
	Describe(ctx context.Context, ec *ExecContext, params json.RawMessage) (title, preview, manual string, err error)
	// Execute performs the action. Output is bounded (<=64 KiB) and masked.
	Execute(ctx context.Context, ec *ExecContext, params json.RawMessage) (output string, err error)
	// Rollback undoes Execute when possible; ErrNoRollback otherwise.
	Rollback(ctx context.Context, ec *ExecContext, params json.RawMessage, executeOutput string) (string, error)
}

// ActionSpec documents an action for the model prompt, the UI and the MCP list.
type ActionSpec struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"` // Chinese
	Description string          `json:"description"`
	Risk        string          `json:"risk"`       // minimum risk
	Channel     string          `json:"channel"`    // k8s | bastion | component | platform
	Params      json.RawMessage `json:"params"`     // JSON Schema
	Permission  string          `json:"permission"` // e.g. "workload:write"
	ReadOnly    bool            `json:"read_only"`
}

// ErrNoRollback is returned by Rollback when the action cannot be undone.
var ErrNoRollback = errNoRollback{}

type errNoRollback struct{}

func (errNoRollback) Error() string { return "action has no automatic rollback" }

// registry of actions; filled in init() of actions_*.go via registerAction.
var actions = map[string]Action{}

func registerAction(a Action) {
	if _, dup := actions[a.Name()]; dup {
		panic("aiops: duplicate action " + a.Name())
	}
	actions[a.Name()] = a
}

// ActionByName returns a registered action.
func ActionByName(name string) (Action, bool) { a, ok := actions[name]; return a, ok }

// ActionSpecs lists every registered action (stable order by name).
func ActionSpecs() []ActionSpec {
	out := make([]ActionSpec, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Spec())
	}
	sortSpecs(out)
	return out
}

// RiskRank orders risks: read < change < high.
func RiskRank(r string) int {
	switch r {
	case RiskRead:
		return 0
	case RiskChange:
		return 1
	default:
		return 2
	}
}

// MaxRisk returns the higher of two risks.
func MaxRisk(a, b string) string {
	if RiskRank(b) > RiskRank(a) {
		return b
	}
	return a
}
