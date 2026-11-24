package aiops

// store.go — persistence for cases, plans (steps as JSONB), timeline and
// runbooks. PostgreSQL via m.d.DB (*pgxpool.Pool). Policy lives in settings
// (policy.go), not here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// errStoreNotFound is returned by get* / update* / delete* when the row does not exist.
var errStoreNotFound = errors.New("aiops: not found")

// caseFilter is the query of GET /aiops/cases. Page is 1-based; Size defaults to 20, max 200.
type caseFilter struct {
	Status string // exact; comma-separated list allowed ("executing,verifying")
	Kind   string // exact
	Q      string // ILIKE on title / request
	Page   int
	Size   int
}

// TimelineEntry is one event on a case's timeline.
type TimelineEntry struct {
	ID        int64           `json:"id"`
	CaseID    int64           `json:"case_id"`
	Kind      string          `json:"kind"`
	Actor     string          `json:"actor"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt time.Time       `json:"created_at"`
}

// ---------- JSON helpers (pure, unit-tested) ----------

// jsonText returns raw as a JSON text suitable for a $n::jsonb parameter;
// empty / invalid input yields def ("{}", "[]", "null").
func jsonText(raw []byte, def string) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || !json.Valid([]byte(s)) {
		return def
	}
	return s
}

// jsonOf marshals v (any; json.RawMessage / []byte passed through) to JSON text, def on nil/error.
func jsonOf(v any, def string) string {
	switch t := v.(type) {
	case nil:
		return def
	case json.RawMessage:
		return jsonText(t, def)
	case []byte:
		return jsonText(t, def)
	case string:
		if json.Valid([]byte(t)) && strings.TrimSpace(t) != "" {
			return t
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return def
	}
	return jsonText(b, def)
}

// rawOr returns b as json.RawMessage, or def when b is empty.
func rawOr(b []byte, def string) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(def)
	}
	return json.RawMessage(append([]byte(nil), b...))
}

// encodeSteps marshals steps (nil -> "[]").
func encodeSteps(steps []Step) (string, error) {
	if steps == nil {
		steps = []Step{}
	}
	b, err := json.Marshal(steps)
	return string(b), err
}

// decodeSteps unmarshals a steps column (empty -> empty slice).
func decodeSteps(b []byte) ([]Step, error) {
	steps := []Step{}
	if len(b) == 0 || string(b) == "null" {
		return steps, nil
	}
	if err := json.Unmarshal(b, &steps); err != nil {
		return nil, fmt.Errorf("aiops: decode steps: %w", err)
	}
	return steps, nil
}

// planRisk returns the max risk of the steps (read when there are none).
func planRisk(steps []Step) string {
	r := RiskRead
	for _, s := range steps {
		if s.Risk != "" {
			r = MaxRisk(r, s.Risk)
		}
	}
	return r
}

// normPage clamps page/size and returns (limit, offset).
func normPage(page, size int) (int, int) {
	if size <= 0 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	if page <= 0 {
		page = 1
	}
	return size, (page - 1) * size
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errStoreNotFound
	}
	return err
}

// ---------- cases ----------

const caseCols = `id,kind,source,title,status,env,cluster_id,alert_id,asset_id,request,target,evidence,conclusion,
mode,plan_id,approval_id,runbook_id,engine,error,created_by,created_name,reviewed_by,created_at,updated_at,resolved_at`

func scanCase(r pgx.Row) (*Case, error) {
	var c Case
	var target, evidence, conclusion []byte
	err := r.Scan(&c.ID, &c.Kind, &c.Source, &c.Title, &c.Status, &c.Env, &c.ClusterID, &c.AlertID, &c.AssetID,
		&c.Request, &target, &evidence, &conclusion, &c.Mode, &c.PlanID, &c.ApprovalID, &c.RunbookID,
		&c.Engine, &c.Error, &c.CreatedBy, &c.CreatedName, &c.ReviewedBy, &c.CreatedAt, &c.UpdatedAt, &c.ResolvedAt)
	if err != nil {
		return nil, notFound(err)
	}
	c.Target = rawOr(target, "{}")
	c.Evidence = rawOr(evidence, "[]")
	c.Conclusion = rawOr(conclusion, "{}")
	return &c, nil
}

// insertCase stores c (ID/CreatedAt/UpdatedAt are filled in). Empty Status
// defaults to investigating, empty Source to manual.
func (m *Module) insertCase(ctx context.Context, c *Case) error {
	if c.Status == "" {
		c.Status = CaseInvestigating
	}
	if c.Source == "" {
		c.Source = SourceManual
	}
	if c.Kind == "" {
		c.Kind = KindRequest
	}
	return m.d.DB.QueryRow(ctx, `INSERT INTO aiops_cases
 (kind,source,title,status,env,cluster_id,alert_id,asset_id,request,target,evidence,conclusion,mode,engine,error,created_by,created_name)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb,$12::jsonb,$13,$14,$15,$16,$17)
 RETURNING id,created_at,updated_at`,
		c.Kind, c.Source, c.Title, c.Status, c.Env, c.ClusterID, c.AlertID, c.AssetID, c.Request,
		jsonText(c.Target, "{}"), jsonText(c.Evidence, "[]"), jsonText(c.Conclusion, "{}"),
		c.Mode, c.Engine, c.Error, c.CreatedBy, c.CreatedName,
	).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
}

// getCase loads one case; errStoreNotFound when missing.
func (m *Module) getCase(ctx context.Context, id int64) (*Case, error) {
	return scanCase(m.d.DB.QueryRow(ctx, `SELECT `+caseCols+` FROM aiops_cases WHERE id=$1`, id))
}

// listCases returns one page (newest first) and the total count for f.
func (m *Module) listCases(ctx context.Context, f caseFilter) ([]Case, int, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if s := strings.TrimSpace(f.Status); s != "" {
		if strings.Contains(s, ",") {
			parts := []string{}
			for _, p := range strings.Split(s, ",") {
				if p = strings.TrimSpace(p); p != "" {
					parts = append(parts, p)
				}
			}
			add("status = ANY($%d)", parts)
		} else {
			add("status = $%d", s)
		}
	}
	if k := strings.TrimSpace(f.Kind); k != "" {
		add("kind = $%d", k)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		args = append(args, "%"+q+"%")
		n := len(args)
		where = append(where, fmt.Sprintf("(title ILIKE $%d OR request ILIKE $%d)", n, n))
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM aiops_cases`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit, offset := normPage(f.Page, f.Size)
	args = append(args, limit, offset)
	rows, err := m.d.DB.Query(ctx, `SELECT `+caseCols+` FROM aiops_cases`+w+
		fmt.Sprintf(` ORDER BY id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Case{}
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *c)
	}
	return items, total, rows.Err()
}

// updateCaseStatus sets status and error (errMsg "" clears it). resolved_at is
// stamped when status becomes resolved/failed/cancelled.
func (m *Module) updateCaseStatus(ctx context.Context, id int64, status, errMsg string) error {
	tag, err := m.d.DB.Exec(ctx, `UPDATE aiops_cases SET status=$2, error=$3, updated_at=now(),
 resolved_at = CASE WHEN $2 IN ('resolved','failed','cancelled') THEN COALESCE(resolved_at, now()) ELSE resolved_at END
 WHERE id=$1`, id, status, errMsg)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errStoreNotFound
	}
	return nil
}

// updateCase writes every mutable field of c (title, status, env, ids, target,
// mode, plan/approval/runbook ids, engine, error, reviewed_by, resolved_at) and
// refreshes c.UpdatedAt. Evidence/conclusion use saveEvidence/saveConclusion.
func (m *Module) updateCase(ctx context.Context, c *Case) error {
	err := m.d.DB.QueryRow(ctx, `UPDATE aiops_cases SET title=$2,status=$3,env=$4,cluster_id=$5,alert_id=$6,asset_id=$7,
 target=$8::jsonb,mode=$9,plan_id=$10,approval_id=$11,runbook_id=$12,engine=$13,error=$14,reviewed_by=$15,resolved_at=$16,updated_at=now()
 WHERE id=$1 RETURNING updated_at`,
		c.ID, c.Title, c.Status, c.Env, c.ClusterID, c.AlertID, c.AssetID, jsonText(c.Target, "{}"),
		c.Mode, c.PlanID, c.ApprovalID, c.RunbookID, c.Engine, c.Error, c.ReviewedBy, c.ResolvedAt,
	).Scan(&c.UpdatedAt)
	return notFound(err)
}

// saveConclusion stores the conclusion (Conclusion struct or raw JSON) and engine ("model"|"rules"; "" keeps it).
func (m *Module) saveConclusion(ctx context.Context, id int64, conclusion any, engine string) error {
	tag, err := m.d.DB.Exec(ctx, `UPDATE aiops_cases SET conclusion=$2::jsonb,
 engine=CASE WHEN $3='' THEN engine ELSE $3 END, updated_at=now() WHERE id=$1`, id, jsonOf(conclusion, "{}"), engine)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errStoreNotFound
	}
	return nil
}

// saveEvidence replaces the evidence list ([]EvidenceItem or raw JSON array).
func (m *Module) saveEvidence(ctx context.Context, id int64, evidence any) error {
	tag, err := m.d.DB.Exec(ctx, `UPDATE aiops_cases SET evidence=$2::jsonb, updated_at=now() WHERE id=$1`,
		id, jsonOf(evidence, "[]"))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errStoreNotFound
	}
	return nil
}

// ---------- plans ----------

const planCols = `id,case_id,seq,title,description,risk,impact,estimate_sec,rollback,status,reject_note,steps,created_at`

func scanPlan(r pgx.Row) (*Plan, error) {
	var p Plan
	var steps []byte
	if err := r.Scan(&p.ID, &p.CaseID, &p.Seq, &p.Title, &p.Description, &p.Risk, &p.Impact, &p.EstimateSec,
		&p.Rollback, &p.Status, &p.RejectNote, &steps, &p.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	s, err := decodeSteps(steps)
	if err != nil {
		return nil, err
	}
	p.Steps = s
	return &p, nil
}

// assignStepIDs gives every step with ID==0 a globally unique id
// (aiops_step_id_seq), sets PlanID and defaults Status/Phase/Executor.
func (m *Module) assignStepIDs(ctx context.Context, p *Plan) error {
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.ID == 0 {
			if err := m.d.DB.QueryRow(ctx, `SELECT nextval('aiops_step_id_seq')`).Scan(&s.ID); err != nil {
				return err
			}
		}
		s.PlanID = p.ID
		fillStepDefaults(s, i)
	}
	return nil
}

// fillStepDefaults sets Seq (when 0), Status=pending, Phase=main, Executor=ai when empty.
func fillStepDefaults(s *Step, i int) {
	if s.Seq == 0 {
		s.Seq = i + 1
	}
	if s.Status == "" {
		s.Status = StepPending
	}
	if s.Phase == "" {
		s.Phase = "main"
	}
	if s.Executor == "" {
		s.Executor = ModeAI
	}
}

// insertPlan stores p for p.CaseID; fills ID, CreatedAt, step ids; empty Risk
// becomes the max risk of the steps, empty Status "proposed".
func (m *Module) insertPlan(ctx context.Context, p *Plan) error {
	if p.Status == "" {
		p.Status = "proposed"
	}
	if p.Risk == "" {
		p.Risk = planRisk(p.Steps)
	}
	err := m.d.DB.QueryRow(ctx, `INSERT INTO aiops_plans
 (case_id,seq,title,description,risk,impact,estimate_sec,rollback,status,reject_note)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id,created_at`,
		p.CaseID, p.Seq, p.Title, p.Description, p.Risk, p.Impact, p.EstimateSec, p.Rollback, p.Status, p.RejectNote,
	).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return err
	}
	if err := m.assignStepIDs(ctx, p); err != nil {
		return err
	}
	steps, err := encodeSteps(p.Steps)
	if err != nil {
		return err
	}
	_, err = m.d.DB.Exec(ctx, `UPDATE aiops_plans SET steps=$2::jsonb WHERE id=$1`, p.ID, steps)
	return err
}

// getPlan loads one plan; errStoreNotFound when missing.
func (m *Module) getPlan(ctx context.Context, id int64) (*Plan, error) {
	return scanPlan(m.d.DB.QueryRow(ctx, `SELECT `+planCols+` FROM aiops_plans WHERE id=$1`, id))
}

// listPlans returns the plans of a case ordered by seq, id.
func (m *Module) listPlans(ctx context.Context, caseID int64) ([]Plan, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT `+planCols+` FROM aiops_plans WHERE case_id=$1 ORDER BY seq, id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// updatePlan writes title, description, risk, impact, estimate, rollback,
// status, reject_note and the full steps array (new steps get ids).
func (m *Module) updatePlan(ctx context.Context, p *Plan) error {
	if err := m.assignStepIDs(ctx, p); err != nil {
		return err
	}
	steps, err := encodeSteps(p.Steps)
	if err != nil {
		return err
	}
	tag, err := m.d.DB.Exec(ctx, `UPDATE aiops_plans SET title=$2,description=$3,risk=$4,impact=$5,estimate_sec=$6,
 rollback=$7,status=$8,reject_note=$9,steps=$10::jsonb,updated_at=now() WHERE id=$1`,
		p.ID, p.Title, p.Description, p.Risk, p.Impact, p.EstimateSec, p.Rollback, p.Status, p.RejectNote, steps)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errStoreNotFound
	}
	return nil
}

// supersedePlans marks every proposed plan of a case as superseded (before a re-investigation).
func (m *Module) supersedePlans(ctx context.Context, caseID int64) error {
	_, err := m.d.DB.Exec(ctx, `UPDATE aiops_plans SET status='superseded', updated_at=now()
 WHERE case_id=$1 AND status='proposed'`, caseID)
	return err
}

// findStep locates a step by id among plans; returns plan, index (-1 when absent).
func findStep(plans []Plan, stepID int64) (*Plan, int) {
	for i := range plans {
		for j := range plans[i].Steps {
			if plans[i].Steps[j].ID == stepID {
				return &plans[i], j
			}
		}
	}
	return nil, -1
}

// ---------- timeline ----------

// appendTimeline adds an event. detail is any JSON-marshalable value (nil -> {});
// callers must not put secrets in it.
func (m *Module) appendTimeline(ctx context.Context, caseID int64, kind, actor string, detail any) error {
	_, err := m.d.DB.Exec(ctx, `INSERT INTO aiops_timeline (case_id,kind,actor,detail) VALUES ($1,$2,$3,$4::jsonb)`,
		caseID, kind, actor, jsonOf(detail, "{}"))
	return err
}

// listTimeline returns a case's events oldest first.
func (m *Module) listTimeline(ctx context.Context, caseID int64) ([]TimelineEntry, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT id,case_id,kind,actor,detail,created_at FROM aiops_timeline
 WHERE case_id=$1 ORDER BY id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TimelineEntry{}
	for rows.Next() {
		var e TimelineEntry
		var d []byte
		if err := rows.Scan(&e.ID, &e.CaseID, &e.Kind, &e.Actor, &d, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Detail = rawOr(d, "{}")
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- runbooks ----------

const runbookCols = `id,name,match,plan,risk,uses,successes,auto_execute,auto_scope,source_case,created_by,updated_at`

func scanRunbook(r pgx.Row) (*Runbook, error) {
	var rb Runbook
	var match, plan, scope []byte
	if err := r.Scan(&rb.ID, &rb.Name, &match, &plan, &rb.Risk, &rb.Uses, &rb.Successes, &rb.AutoExecute,
		&scope, &rb.SourceCase, &rb.CreatedBy, &rb.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	rb.Match = rawOr(match, "{}")
	rb.AutoScope = rawOr(scope, "{}")
	if len(plan) > 0 {
		if err := json.Unmarshal(plan, &rb.Plan); err != nil {
			return nil, fmt.Errorf("aiops: decode runbook plan: %w", err)
		}
	}
	return &rb, nil
}

// encodeRunbookPlan marshals a PlanDraft (nil step slices -> []).
func encodeRunbookPlan(p PlanDraft) (string, error) {
	if p.Steps == nil {
		p.Steps = []StepDraft{}
	}
	if p.Verify == nil {
		p.Verify = []StepDraft{}
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// insertRunbook stores rb; fills ID and UpdatedAt. AutoExecute is forced false when Risk is high.
func (m *Module) insertRunbook(ctx context.Context, rb *Runbook) error {
	if rb.Risk == "" {
		rb.Risk = RiskRead
	}
	if rb.Risk == RiskHigh {
		rb.AutoExecute = false
	}
	plan, err := encodeRunbookPlan(rb.Plan)
	if err != nil {
		return err
	}
	return m.d.DB.QueryRow(ctx, `INSERT INTO aiops_runbooks
 (name,match,plan,risk,uses,successes,auto_execute,auto_scope,source_case,created_by)
 VALUES ($1,$2::jsonb,$3::jsonb,$4,$5,$6,$7,$8::jsonb,$9,$10) RETURNING id,updated_at`,
		rb.Name, jsonText(rb.Match, "{}"), plan, rb.Risk, rb.Uses, rb.Successes, rb.AutoExecute,
		jsonText(rb.AutoScope, "{}"), rb.SourceCase, rb.CreatedBy,
	).Scan(&rb.ID, &rb.UpdatedAt)
}

// listRunbooks returns all runbooks, newest updated first.
func (m *Module) listRunbooks(ctx context.Context) ([]Runbook, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT `+runbookCols+` FROM aiops_runbooks ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Runbook{}
	for rows.Next() {
		rb, err := scanRunbook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rb)
	}
	return out, rows.Err()
}

// getRunbook loads one runbook; errStoreNotFound when missing.
func (m *Module) getRunbook(ctx context.Context, id int64) (*Runbook, error) {
	return scanRunbook(m.d.DB.QueryRow(ctx, `SELECT `+runbookCols+` FROM aiops_runbooks WHERE id=$1`, id))
}

// updateRunbook writes name, match, plan, risk, uses, successes, auto_execute,
// auto_scope (AutoExecute forced false when Risk is high); refreshes UpdatedAt.
func (m *Module) updateRunbook(ctx context.Context, rb *Runbook) error {
	if rb.Risk == RiskHigh {
		rb.AutoExecute = false
	}
	plan, err := encodeRunbookPlan(rb.Plan)
	if err != nil {
		return err
	}
	err = m.d.DB.QueryRow(ctx, `UPDATE aiops_runbooks SET name=$2,match=$3::jsonb,plan=$4::jsonb,risk=$5,uses=$6,
 successes=$7,auto_execute=$8,auto_scope=$9::jsonb,updated_at=now() WHERE id=$1 RETURNING updated_at`,
		rb.ID, rb.Name, jsonText(rb.Match, "{}"), plan, rb.Risk, rb.Uses, rb.Successes, rb.AutoExecute,
		jsonText(rb.AutoScope, "{}"),
	).Scan(&rb.UpdatedAt)
	return notFound(err)
}

// bumpRunbookUse increments uses (and successes when success) after a case executed from it.
func (m *Module) bumpRunbookUse(ctx context.Context, id int64, success bool) error {
	_, err := m.d.DB.Exec(ctx, `UPDATE aiops_runbooks SET uses=uses+1,
 successes=successes+CASE WHEN $2 THEN 1 ELSE 0 END WHERE id=$1`, id, success)
	return err
}

// deleteRunbook removes a runbook; errStoreNotFound when missing.
func (m *Module) deleteRunbook(ctx context.Context, id int64) error {
	tag, err := m.d.DB.Exec(ctx, `DELETE FROM aiops_runbooks WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errStoreNotFound
	}
	return nil
}
