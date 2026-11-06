package observability

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Non-alert events are collected by polling append-only tables with a cursor
// per source, so the producers (on-call handlers in this module, the CI/CD
// module, the core approval service) need no code changes:
//
//	incident  oncall_incident_events (declared / assigned / acknowledged / resolved)
//	approval  approval_tickets       (new tickets still pending when seen)
//	pipeline  cicd_runs              (finished runs: succeeded / failed / cancelled)
//
// A cursor is advanced with compare-and-set before dispatching, so with
// several replicas each row is notified by exactly one of them. A fresh cursor
// starts at "now": history is never replayed.
const (
	notifyPollInterval = 15 * time.Second
	notifyRetention    = 30 * 24 * time.Hour
	notifyPollBatch    = 200
)

// startNotify runs the event collector until ctx is cancelled.
func (m *Module) startNotify(ctx context.Context) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		tick := time.NewTicker(notifyPollInterval)
		defer tick.Stop()
		lastCleanup := time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			m.pollNotifyEvents(ctx)
			if time.Since(lastCleanup) > time.Hour {
				lastCleanup = time.Now()
				if _, err := m.d.DB.Exec(ctx, `DELETE FROM obs_notify_records WHERE created_at < $1`, time.Now().Add(-notifyRetention)); err != nil {
					m.logWarn("notify record retention", err)
				}
			}
		}
	}()
}

func (m *Module) pollNotifyEvents(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	link := strings.TrimRight(m.aiopsPolicy(ctx).PublicURL, "/")
	for name, fn := range map[string]func(context.Context, string) ([]*NotifyEvent, error){
		"incident": m.collectIncidents, "approval": m.collectApprovals, "pipeline": m.collectPipelines,
	} {
		evs, err := fn(ctx, link)
		if err != nil {
			m.logWarn("collect "+name+" notifications", err)
			continue
		}
		m.dispatch(ctx, evs)
	}
}

type notifyCursor struct {
	ID int64
	At time.Time
}

// cursor returns the stored position; ok=false means it was just initialised.
func (m *Module) cursor(ctx context.Context, name, initSQL string) (notifyCursor, bool, error) {
	var c notifyCursor
	err := m.d.DB.QueryRow(ctx, `SELECT last_id,last_at FROM obs_notify_cursors WHERE name=$1`, name).Scan(&c.ID, &c.At)
	if err == nil {
		return c, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return c, false, err
	}
	if err := m.d.DB.QueryRow(ctx, initSQL).Scan(&c.ID, &c.At); err != nil {
		return c, false, err
	}
	_, err = m.d.DB.Exec(ctx, `INSERT INTO obs_notify_cursors (name,last_id,last_at) VALUES ($1,$2,$3) ON CONFLICT (name) DO NOTHING`, name, c.ID, c.At)
	return c, false, err
}

// advance moves the cursor from old to next; false means another replica won.
func (m *Module) advance(ctx context.Context, name string, old, next notifyCursor) bool {
	tag, err := m.d.DB.Exec(ctx, `UPDATE obs_notify_cursors SET last_id=$2,last_at=$3,updated_at=now() WHERE name=$1 AND last_id=$4 AND last_at=$5`,
		name, next.ID, next.At, old.ID, old.At)
	return err == nil && tag.RowsAffected() == 1
}

func (m *Module) tableExists(ctx context.Context, name string) bool {
	var ok bool
	_ = m.d.DB.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok)
	return ok
}

// incidentEvent maps one on-call timeline row (pure, unit-tested).
func incidentEvent(eventID, incidentID int64, kind, actor, message, title, description, priority, service, env, assignee string, declaredAt time.Time, link string) *NotifyEvent {
	ev := &NotifyEvent{Kind: "incident", Status: kind, ID: incidentID, AlertName: title, SeverityRaw: priority,
		Severity: normSeverity(priority), Env: env, Resource: service, Actor: actor, StartsAtT: declaredAt,
		Key:    "incident:" + strconv.FormatInt(incidentID, 10) + ":" + kind + ":" + strconv.FormatInt(eventID, 10),
		Labels: map[string]string{"incident_id": strconv.FormatInt(incidentID, 10), "priority": priority}}
	if service != "" {
		ev.Labels["service"] = service
	}
	if assignee != "" {
		ev.Labels["assigned_to"] = assignee
	}
	ev.Summary = message
	if kind == "declared" && description != "" {
		ev.Summary = description
	}
	if link != "" {
		ev.Link = link + "/#/oncall/inc/" + strconv.FormatInt(incidentID, 10)
	}
	return ev
}

func (m *Module) collectIncidents(ctx context.Context, link string) ([]*NotifyEvent, error) {
	const name = "incident"
	cur, ok, err := m.cursor(ctx, name, `SELECT COALESCE(max(id),0), now() FROM oncall_incident_events`)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := m.d.DB.Query(ctx, `SELECT e.id,e.kind,e.actor,e.message,i.id,i.title,i.description,i.severity,i.service,i.env,i.assigned_to,i.declared_at
		FROM oncall_incident_events e JOIN oncall_incidents i ON i.id=e.incident_id WHERE e.id > $1 ORDER BY e.id LIMIT $2`, cur.ID, notifyPollBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	next := cur
	var out []*NotifyEvent
	for rows.Next() {
		var eid, iid int64
		var kind, actor, msg, title, desc, prio, svc, env, assignee string
		var declared time.Time
		if err := rows.Scan(&eid, &kind, &actor, &msg, &iid, &title, &desc, &prio, &svc, &env, &assignee, &declared); err != nil {
			return nil, err
		}
		next.ID = eid
		switch kind {
		case "declared", "assigned", "acknowledged", "resolved":
			out = append(out, incidentEvent(eid, iid, kind, actor, msg, title, desc, prio, svc, env, assignee, declared, link))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if (next.ID == cur.ID && next.At.Equal(cur.At)) || !m.advance(ctx, name, cur, next) {
		return nil, nil
	}
	return out, nil
}

func approvalEvent(id int64, typ, title, env, reason, applicant string, created time.Time, link string) *NotifyEvent {
	sev := "info"
	if strings.EqualFold(env, "prod") || strings.EqualFold(env, "production") {
		sev = "warning"
	}
	ev := &NotifyEvent{Kind: "approval", Status: "pending", ID: id, AlertName: title, Severity: sev, Env: env,
		Summary: reason, Actor: applicant, StartsAtT: created, Key: "approval:" + strconv.FormatInt(id, 10),
		Labels: map[string]string{"type": typ, "applicant": applicant, "ticket_id": strconv.FormatInt(id, 10)}}
	if link != "" {
		ev.Link = link + "/#/approvals"
	}
	return ev
}

func (m *Module) collectApprovals(ctx context.Context, link string) ([]*NotifyEvent, error) {
	const name = "approval"
	if !m.tableExists(ctx, "approval_tickets") {
		return nil, nil
	}
	cur, ok, err := m.cursor(ctx, name, `SELECT COALESCE(max(id),0), now() FROM approval_tickets`)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := m.d.DB.Query(ctx, `SELECT id,type,title,env,reason,status,applicant_name,created_at FROM approval_tickets WHERE id > $1 ORDER BY id LIMIT $2`,
		cur.ID, notifyPollBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	next := cur
	var out []*NotifyEvent
	for rows.Next() {
		var id int64
		var typ, title, env, reason, status, applicant string
		var created time.Time
		if err := rows.Scan(&id, &typ, &title, &env, &reason, &status, &applicant, &created); err != nil {
			return nil, err
		}
		next.ID = id
		if status == "pending" { // already decided tickets are no longer a to-do
			out = append(out, approvalEvent(id, typ, title, env, reason, applicant, created, link))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if (next.ID == cur.ID && next.At.Equal(cur.At)) || !m.advance(ctx, name, cur, next) {
		return nil, nil
	}
	return out, nil
}

func pipelineEvent(id int64, status, trigger, by, branch, commit, errMsg, pipeline, app, cluster, env string, started, finished *time.Time, link string) *NotifyEvent {
	sev := "info"
	if status == "failed" {
		sev = "warning"
	}
	name := pipeline
	if app != "" {
		name = app + " / " + pipeline
	}
	ev := &NotifyEvent{Kind: "pipeline", Status: status, ID: id, AlertName: name, Severity: sev, Cluster: cluster, Env: env,
		Actor: by, Summary: errMsg, Key: "pipeline:" + strconv.FormatInt(id, 10),
		Labels: map[string]string{"app": app, "pipeline": pipeline, "branch": branch, "trigger": trigger, "run_id": strconv.FormatInt(id, 10)}}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if commit != "" {
		ev.Labels["commit"] = commit
	}
	if started != nil {
		ev.StartsAtT = *started
	}
	if started != nil && finished != nil && ev.Summary == "" {
		ev.Summary = "耗时 " + finished.Sub(*started).Round(time.Second).String()
	}
	if link != "" {
		ev.Link = link + "/#/cicd/pipeline"
	}
	return ev
}

func (m *Module) collectPipelines(ctx context.Context, link string) ([]*NotifyEvent, error) {
	const name = "pipeline"
	if !m.tableExists(ctx, "cicd_runs") {
		return nil, nil
	}
	cur, ok, err := m.cursor(ctx, name, `SELECT 0::bigint, now()`)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := m.d.DB.Query(ctx, `SELECT r.id,r.status,r.trigger,r.triggered_by,r.branch,r.commit_sha,r.error,r.started_at,r.finished_at,
		p.name, COALESCE(a.name,''), COALESCE(cl.name,''), COALESCE(cl.env,'')
		FROM cicd_runs r JOIN cicd_pipelines p ON p.id=r.pipeline_id
		LEFT JOIN cicd_applications a ON a.id=r.app_id
		LEFT JOIN cicd_targets t ON t.id=p.runner_target_id
		LEFT JOIN cluster_clusters cl ON cl.id=t.cluster_id
		WHERE r.finished_at IS NOT NULL AND r.status IN ('succeeded','failed','cancelled') AND (r.finished_at, r.id) > ($1, $2)
		ORDER BY r.finished_at, r.id LIMIT $3`, cur.At, cur.ID, notifyPollBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	next := cur
	var out []*NotifyEvent
	for rows.Next() {
		var id int64
		var status, trigger, by, branch, commit, errMsg, pipeline, app, cluster, env string
		var started, finished *time.Time
		if err := rows.Scan(&id, &status, &trigger, &by, &branch, &commit, &errMsg, &started, &finished, &pipeline, &app, &cluster, &env); err != nil {
			return nil, err
		}
		next = notifyCursor{ID: id, At: *finished}
		out = append(out, pipelineEvent(id, status, trigger, by, branch, commit, errMsg, pipeline, app, cluster, env, started, finished, link))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if (next.ID == cur.ID && next.At.Equal(cur.At)) || !m.advance(ctx, name, cur, next) {
		return nil, nil
	}
	return out, nil
}
