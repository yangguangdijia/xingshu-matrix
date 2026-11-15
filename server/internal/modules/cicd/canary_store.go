package cicd

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// Canary is one canary release of an application environment (table cicd_canaries).
type Canary struct {
	ID               int64          `json:"id"`
	AppID            int64          `json:"app_id"`
	Env              string         `json:"env"`
	TargetID         int64          `json:"target_id"`
	TargetConfig     string         `json:"-"`
	ReleaseID        *int64         `json:"release_id"`
	Image            string         `json:"image"`
	StableImage      string         `json:"stable_image"`
	Namespace        string         `json:"namespace"`
	Deployment       string         `json:"deployment"`
	Container        string         `json:"container"`
	Service          string         `json:"service"`
	Ingress          string         `json:"ingress"`
	CanaryDeployment string         `json:"canary_deployment"`
	CanaryService    string         `json:"canary_service"`
	CanaryIngress    string         `json:"canary_ingress"`
	Plan             canaryPlan     `json:"plan"`
	Status           string         `json:"status"`
	CurrentStep      int            `json:"current_step"`
	Weight           int            `json:"weight"`
	StepStartedAt    *time.Time     `json:"step_started_at"`
	NextAt           time.Time      `json:"next_at"`
	Attempts         int            `json:"attempts"`
	Message          string         `json:"message"`
	ApprovalID       *int64         `json:"approval_id"`
	PendingAction    string         `json:"pending_action"`
	Reason           string         `json:"reason"`
	CreatedBy        string         `json:"created_by"`
	CreatedByID      int64          `json:"-"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	FinishedAt       *time.Time     `json:"finished_at"`
	Events           []*CanaryEvent `json:"events,omitempty"`
}

// CanaryEvent is one entry of a canary's timeline.
type CanaryEvent struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"` // info|step|operator|error
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Actor   string          `json:"actor"`
	TS      time.Time       `json:"ts"`
}

const canaryCols = `id,app_id,env,target_id,target_config,release_id,image,stable_image,namespace,deployment,container,
	service,ingress,canary_deployment,canary_service,canary_ingress,plan,status,current_step,weight,step_started_at,next_at,
	attempts,message,approval_id,pending_action,reason,created_by,created_by_id,created_at,updated_at,finished_at`

func scanCanary(row pgx.Row) (*Canary, error) {
	var c Canary
	var plan []byte
	err := row.Scan(&c.ID, &c.AppID, &c.Env, &c.TargetID, &c.TargetConfig, &c.ReleaseID, &c.Image, &c.StableImage,
		&c.Namespace, &c.Deployment, &c.Container, &c.Service, &c.Ingress, &c.CanaryDeployment, &c.CanaryService,
		&c.CanaryIngress, &plan, &c.Status, &c.CurrentStep, &c.Weight, &c.StepStartedAt, &c.NextAt, &c.Attempts,
		&c.Message, &c.ApprovalID, &c.PendingAction, &c.Reason, &c.CreatedBy, &c.CreatedByID, &c.CreatedAt,
		&c.UpdatedAt, &c.FinishedAt)
	if err != nil {
		return nil, err
	}
	if len(plan) > 0 {
		_ = json.Unmarshal(plan, &c.Plan)
	}
	return &c, nil
}

func (m *Module) loadCanary(ctx context.Context, id int64) (*Canary, error) {
	c, err := scanCanary(m.db.QueryRow(ctx, `SELECT `+canaryCols+` FROM cicd_canaries WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("金丝雀发布不存在")
	}
	return c, err
}

func (m *Module) listCanaryRows(ctx context.Context, appID int64, env string) ([]*Canary, error) {
	q, args := `SELECT `+canaryCols+` FROM cicd_canaries WHERE app_id=$1`, []any{appID}
	if env != "" {
		q, args = q+` AND env=$2`, append(args, env)
	}
	rows, err := m.db.Query(ctx, q+` ORDER BY id DESC LIMIT 100`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Canary{}
	for rows.Next() {
		c, err := scanCanary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// insertCanary reserves the application environment: the partial unique index
// cicd_canaries_active_uniq allows one unfinished canary per app/env.
func (m *Module) insertCanary(ctx context.Context, c *Canary) error {
	plan, _ := json.Marshal(c.Plan)
	err := m.db.QueryRow(ctx, `INSERT INTO cicd_canaries(app_id,env,target_id,target_config,image,stable_image,namespace,
		deployment,container,service,ingress,canary_deployment,canary_service,canary_ingress,plan,status,reason,created_by,created_by_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) RETURNING id,created_at,updated_at,next_at`,
		c.AppID, c.Env, c.TargetID, c.TargetConfig, c.Image, c.StableImage, c.Namespace, c.Deployment, c.Container,
		c.Service, c.Ingress, c.CanaryDeployment, c.CanaryService, c.CanaryIngress, plan, c.Status, c.Reason,
		c.CreatedBy, c.CreatedByID).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt, &c.NextAt)
	if err != nil && isUniqueViolation(err) {
		return httpx.Conflict("该应用环境已有进行中的金丝雀发布")
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pe interface{ SQLState() string }
	return errors.As(err, &pe) && pe.SQLState() == "23505"
}

// canaryUpdate is a partial update of the state columns.
type canaryUpdate struct {
	Status        *string
	Step          *int
	Weight        *int
	NextAt        *time.Time
	Message       *string
	ApprovalID    *int64 // 0 clears
	PendingAction *string
	StableImage   *string
	ReleaseID     *int64
	Finished      bool
	StepStarted   bool
	ResetAttempts bool // attempts=1 (the current claim) when a new operation starts
}

func (m *Module) updateCanary(ctx context.Context, id int64, u canaryUpdate) error {
	set, args := `updated_at=now()`, []any{id}
	add := func(col string, v any) {
		args = append(args, v)
		set += `,` + col + `=$` + strconv.Itoa(len(args))
	}
	if u.Status != nil {
		add("status", *u.Status)
	}
	if u.Step != nil {
		add("current_step", *u.Step)
	}
	if u.Weight != nil {
		add("weight", *u.Weight)
	}
	if u.NextAt != nil {
		add("next_at", *u.NextAt)
	}
	if u.Message != nil {
		add("message", truncRunes(*u.Message, 1000))
	}
	if u.ApprovalID != nil {
		if *u.ApprovalID == 0 {
			set += `,approval_id=NULL`
		} else {
			add("approval_id", *u.ApprovalID)
		}
	}
	if u.PendingAction != nil {
		add("pending_action", *u.PendingAction)
	}
	if u.StableImage != nil {
		add("stable_image", *u.StableImage)
	}
	if u.ReleaseID != nil {
		add("release_id", *u.ReleaseID)
	}
	if u.StepStarted {
		set += `,step_started_at=now()`
	}
	if u.ResetAttempts {
		set += `,attempts=1`
	}
	if u.Finished {
		set += `,finished_at=now()`
	}
	_, err := m.db.Exec(ctx, `UPDATE cicd_canaries SET `+set+` WHERE id=$1`, args...)
	return err
}

// claimCanary takes the row lease for one operation; false when another
// operation (API call or worker) holds it.
func (m *Module) claimCanary(ctx context.Context, id int64, owner string, d time.Duration) (bool, error) {
	tag, err := m.db.Exec(ctx, `UPDATE cicd_canaries SET lease_owner=$2, lease_until=now()+make_interval(secs => $3),
		attempts=attempts+1 WHERE id=$1 AND (lease_until IS NULL OR lease_until < now())`, id, owner, d.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (m *Module) releaseCanary(id int64, owner string) {
	_, _ = m.db.Exec(context.Background(), `UPDATE cicd_canaries SET lease_owner='', lease_until=NULL
		WHERE id=$1 AND lease_owner=$2`, id, owner)
}

// dueCanaries returns progressing canaries whose step timer elapsed and
// in-flight operations whose lease expired (process restart).
func (m *Module) dueCanaries(ctx context.Context) ([]int64, error) {
	rows, err := m.db.Query(ctx, `SELECT id FROM cicd_canaries WHERE
		(status='progressing' AND next_at <= now() AND (lease_until IS NULL OR lease_until < now()))
		OR (status IN ('starting','promoting','rolling_back') AND next_at <= now() AND (lease_until IS NULL OR lease_until < now()))
		ORDER BY next_at LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (m *Module) addCanaryEvent(ctx context.Context, id int64, kind, msg, actor string, data any) {
	if m.db == nil {
		return
	}
	b := []byte("{}")
	if data != nil {
		if raw, err := json.Marshal(data); err == nil {
			b = raw
		}
	}
	if _, err := m.db.Exec(ctx, `INSERT INTO cicd_canary_events(canary_id,kind,message,data,actor) VALUES($1,$2,$3,$4,$5)`,
		id, kind, truncRunes(msg, 1000), b, actor); err != nil && m.log != nil {
		m.log.Warn("canary event", "canary", id, "err", err)
	}
}

func (m *Module) canaryEvents(ctx context.Context, id int64) ([]*CanaryEvent, error) {
	rows, err := m.db.Query(ctx, `SELECT id,kind,message,data,actor,ts FROM cicd_canary_events WHERE canary_id=$1 ORDER BY id LIMIT 500`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CanaryEvent{}
	for rows.Next() {
		var e CanaryEvent
		if err := rows.Scan(&e.ID, &e.Kind, &e.Message, &e.Data, &e.Actor, &e.TS); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func canaryPtr[T any](v T) *T { return &v }
