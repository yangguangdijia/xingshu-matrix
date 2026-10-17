package cicd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Run is one execution of a pipeline.
type Run struct {
	ID            int64       `json:"id"`
	PipelineID    int64       `json:"pipeline_id"`
	AppID         int64       `json:"app_id"`
	Status        string      `json:"status"`
	Trigger       string      `json:"trigger"`
	TriggeredByID int64       `json:"triggered_by_id"`
	TriggeredBy   string      `json:"triggered_by"`
	Branch        string      `json:"branch"`
	Commit        string      `json:"commit"`
	ImageTag      string      `json:"image_tag"`
	Error         string      `json:"error"`
	CreatedAt     time.Time   `json:"created_at"`
	StartedAt     *time.Time  `json:"started_at"`
	FinishedAt    *time.Time  `json:"finished_at"`
	Stages        []*RunStage `json:"stages,omitempty"`
}

// RunStage is the state of one stage of a run.
type RunStage struct {
	Idx        int        `json:"idx"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Status     string     `json:"status"`
	JobName    string     `json:"job_name"`
	ApprovalID *int64     `json:"approval_id"`
	Error      string     `json:"error"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

const runCols = `id,pipeline_id,app_id,status,trigger,triggered_by_id,triggered_by,branch,commit_sha,image_tag,error,created_at,started_at,finished_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	if err := row.Scan(&r.ID, &r.PipelineID, &r.AppID, &r.Status, &r.Trigger, &r.TriggeredByID, &r.TriggeredBy, &r.Branch,
		&r.Commit, &r.ImageTag, &r.Error, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
		return nil, notFound(err)
	}
	return &r, nil
}

func terminal(s string) bool { return s == "succeeded" || s == "failed" || s == "cancelled" }

type runIn struct {
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// startRun validates and enqueues a run; the dispatcher picks it up.
func (m *Module) startRun(ctx context.Context, p *rbac.Principal, pipelineID int64, in runIn, trigger string) (*Run, error) {
	pl, err := m.loadPipe(ctx, pipelineID)
	if err != nil {
		return nil, err
	}
	a, err := m.loadApp(ctx, pl.AppID)
	if err != nil {
		return nil, err
	}
	var uid int64
	uname := "webhook"
	if p != nil {
		uid, uname = p.UserID, p.Username
		for _, s := range pl.Stages { // deploy stages: caller must be scoped to the env
			if s.Type == "deploy" && !p.CanEnv(s.DeployEnv) {
				return nil, httpx.Forbidden("environment not in your scope: " + s.DeployEnv)
			}
		}
	}
	if in.Branch == "" {
		in.Branch = a.Branch
	}
	var r *Run
	err = pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		var e error
		r, e = scanRun(tx.QueryRow(ctx, `INSERT INTO cicd_runs(pipeline_id,app_id,trigger,triggered_by_id,triggered_by,branch,commit_sha)
			VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+runCols, pl.ID, pl.AppID, trigger, uid, uname, in.Branch, in.Commit))
		if e != nil {
			return e
		}
		tag := fmt.Sprintf("run-%d", r.ID)
		if len(in.Commit) >= 7 {
			tag = in.Commit[:min(12, len(in.Commit))]
		}
		if _, e = tx.Exec(ctx, `UPDATE cicd_runs SET image_tag=$2 WHERE id=$1`, r.ID, tag); e != nil {
			return e
		}
		r.ImageTag = tag
		for i, s := range pl.Stages {
			if _, e = tx.Exec(ctx, `INSERT INTO cicd_run_stages(run_id,idx,name,type) VALUES($1,$2,$3,$4)`, r.ID, i, s.Name, s.Type); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return r, nil
}

func (m *Module) runPipelineHTTP(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in runIn
	if c.Request.ContentLength > 0 && !httpx.Bind(c, &in) {
		return
	}
	r, err := m.startRun(c, rbac.PrincipalFrom(c), id, in, "manual")
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_run", fmt.Sprint(r.ID))
	httpx.Created(c, r)
}

func (m *Module) queryRuns(ctx context.Context, pipelineID, appID int64, status string, limit, offset int) ([]*Run, int64, error) {
	where, args := []string{"true"}, []any{}
	add := func(cond string, v any) { args = append(args, v); where = append(where, fmt.Sprintf(cond, len(args))) }
	if pipelineID > 0 {
		add("pipeline_id=$%d", pipelineID)
	}
	if appID > 0 {
		add("app_id=$%d", appID)
	}
	if status != "" {
		add("status=$%d", status)
	}
	w := strings.Join(where, " AND ")
	var total int64
	if err := m.db.QueryRow(ctx, `SELECT count(*) FROM cicd_runs WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := m.db.Query(ctx, fmt.Sprintf(`SELECT `+runCols+` FROM cicd_runs WHERE %s ORDER BY id DESC LIMIT $%d OFFSET $%d`, w, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (m *Module) listRuns(c *gin.Context) {
	p := httpx.ParsePage(c)
	var pid, aid int64
	fmt.Sscan(c.Query("pipeline_id"), &pid)
	fmt.Sscan(c.Query("app_id"), &aid)
	runs, total, err := m.queryRuns(c, pid, aid, c.Query("status"), p.Limit(), p.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageResult(runs, total, p))
}

func (m *Module) loadRun(ctx context.Context, id int64) (*Run, error) {
	r, err := scanRun(m.db.QueryRow(ctx, `SELECT `+runCols+` FROM cicd_runs WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	rows, err := m.db.Query(ctx, `SELECT idx,name,type,status,job_name,approval_id,error,started_at,finished_at FROM cicd_run_stages WHERE run_id=$1 ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s RunStage
		if err := rows.Scan(&s.Idx, &s.Name, &s.Type, &s.Status, &s.JobName, &s.ApprovalID, &s.Error, &s.StartedAt, &s.FinishedAt); err != nil {
			return nil, err
		}
		r.Stages = append(r.Stages, &s)
	}
	return r, rows.Err()
}

func (m *Module) getRun(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	r, err := m.loadRun(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, r)
}

type logLine struct {
	ID    int64     `json:"id"`
	Stage int       `json:"stage"`
	Line  string    `json:"line"`
	TS    time.Time `json:"ts"`
}

func (m *Module) logsAfter(ctx context.Context, runID, after int64, limit int) ([]logLine, error) {
	rows, err := m.db.Query(ctx, `SELECT id,stage_idx,line,ts FROM cicd_run_logs WHERE run_id=$1 AND id>$2 ORDER BY id LIMIT $3`, runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []logLine{}
	for rows.Next() {
		var l logLine
		if err := rows.Scan(&l.ID, &l.Stage, &l.Line, &l.TS); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// runLogs returns logs as JSON, or streams them as SSE when the client asks for
// text/event-stream or ?follow=1. Resumes after Last-Event-ID / ?after.
func (m *Module) runLogs(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var after int64
	fmt.Sscan(c.Query("after"), &after)
	if v := c.GetHeader("Last-Event-ID"); v != "" {
		fmt.Sscan(v, &after)
	}
	if _, err := scanRun(m.db.QueryRow(c, `SELECT `+runCols+` FROM cicd_runs WHERE id=$1`, id)); err != nil {
		httpx.Fail(c, err)
		return
	}
	if c.Query("follow") != "1" && !strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
		lines, err := m.logsAfter(c, id, after, 5000)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, lines)
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	ctx := c.Request.Context()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	idle := 0
	c.Stream(func(w io.Writer) bool {
		lines, err := m.logsAfter(ctx, id, after, 1000)
		if err != nil {
			c.SSEvent("error", err.Error())
			return false
		}
		for _, l := range lines {
			c.Render(-1, sse.Event{Id: fmt.Sprint(l.ID), Event: "log", Data: l})
			after = l.ID
		}
		if len(lines) == 1000 {
			return true
		}
		var status string
		_ = m.db.QueryRow(ctx, `SELECT status FROM cicd_runs WHERE id=$1`, id).Scan(&status)
		if terminal(status) {
			c.SSEvent("end", gin.H{"status": status})
			return false
		}
		if idle++; idle%15 == 0 {
			_, _ = io.WriteString(w, ": ping\n\n")
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
			return true
		}
	})
}

func (m *Module) cancelRun(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var status string
	err := m.db.QueryRow(c, `UPDATE cicd_runs SET status = CASE WHEN status='queued' THEN 'cancelled' ELSE 'cancelling' END,
		finished_at = CASE WHEN status='queued' THEN now() ELSE finished_at END
		WHERE id=$1 AND status IN ('queued','running') RETURNING status`, id).Scan(&status)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("run is not active").Wrap(err))
		return
	}
	if status == "cancelled" {
		_, _ = m.db.Exec(c, `UPDATE cicd_run_stages SET status='cancelled' WHERE run_id=$1 AND status='pending'`, id)
	}
	m.mu.Lock()
	if cancel := m.cancels[id]; cancel != nil {
		cancel()
	}
	m.mu.Unlock()
	audit.SetResource(c, "cicd_run", fmt.Sprint(id))
	httpx.OK(c, gin.H{"id": id, "status": status})
}

// ---------------------------------------------------------------- dispatcher

func (m *Module) dispatch(ctx context.Context) {
	sem := make(chan struct{}, maxConcurrentRuns)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		for {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			var id int64
			err := m.db.QueryRow(ctx, `UPDATE cicd_runs SET status='running', started_at=now()
				WHERE id=(SELECT id FROM cicd_runs WHERE status='queued' ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING id`).Scan(&id)
			if err != nil {
				<-sem
				if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
					m.log.Error("claim run", "err", err)
				}
				break
			}
			m.wg.Add(1)
			go func() {
				defer func() { <-sem; m.wg.Done() }()
				m.execute(ctx, id)
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-m.wake:
		}
	}
}

// logWriter batches log lines into cicd_run_logs.
type logWriter struct {
	m     *Module
	runID int64
	mu    sync.Mutex
	buf   [][]any
}

func (w *logWriter) add(stage int, line string) {
	line = strings.TrimRight(line, "\r\n")
	if len(line) > 8000 {
		line = line[:8000] + "…"
	}
	w.mu.Lock()
	w.buf = append(w.buf, []any{w.runID, stage, strings.ToValidUTF8(line, "?"), time.Now()})
	full := len(w.buf) >= 200
	w.mu.Unlock()
	if full {
		w.flush()
	}
}

func (w *logWriter) stage(idx int) func(string, ...any) {
	return func(f string, a ...any) { w.add(idx, fmt.Sprintf(f, a...)) }
}

func (w *logWriter) flush() {
	w.mu.Lock()
	rows := w.buf
	w.buf = nil
	w.mu.Unlock()
	if len(rows) == 0 {
		return
	}
	_, err := w.m.db.CopyFrom(context.Background(), pgx.Identifier{"cicd_run_logs"}, []string{"run_id", "stage_idx", "line", "ts"}, pgx.CopyFromRows(rows))
	if err != nil {
		w.m.log.Error("write run logs", "run", w.runID, "err", err)
	}
}

func (m *Module) setStage(runID int64, idx int, status, job, errMsg string) {
	q := `UPDATE cicd_run_stages SET status=$3, job_name=COALESCE(NULLIF($4,''),job_name), error=$5,
		started_at=CASE WHEN $3='running' THEN now() ELSE started_at END,
		finished_at=CASE WHEN $3 IN ('succeeded','failed','cancelled','skipped','rejected') THEN now() ELSE finished_at END
		WHERE run_id=$1 AND idx=$2`
	if _, err := m.db.Exec(context.Background(), q, runID, idx, status, job, errMsg); err != nil {
		m.log.Error("update stage", "run", runID, "idx", idx, "err", err)
	}
}

// execute runs every stage of a claimed run in order.
func (m *Module) execute(parent context.Context, runID int64) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	m.mu.Lock()
	m.cancels[runID] = cancel
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.cancels, runID); m.mu.Unlock() }()

	// Observe cancellation requested from another replica.
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				var st string
				if m.db.QueryRow(ctx, `SELECT status FROM cicd_runs WHERE id=$1`, runID).Scan(&st) == nil && st == "cancelling" {
					cancel()
					return
				}
			}
		}
	}()

	lw := &logWriter{m: m, runID: runID}
	flushDone := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-flushDone:
				return
			case <-t.C:
				lw.flush()
			}
		}
	}()

	err := m.runStages(ctx, runID, lw)
	close(flushDone)

	status, msg := "succeeded", ""
	switch {
	case err == nil:
	case ctx.Err() != nil && parent.Err() == nil:
		status, msg = "cancelled", "cancelled by user"
	default:
		status, msg = "failed", err.Error()
	}
	if err != nil {
		lw.add(-1, "run "+status+": "+msg)
	}
	lw.flush()
	_, _ = m.db.Exec(context.Background(), `UPDATE cicd_run_stages SET status=$2 WHERE run_id=$1 AND status IN ('pending','awaiting_approval','approved')`,
		runID, map[bool]string{true: "cancelled", false: "skipped"}[status == "cancelled"])
	_, _ = m.db.Exec(context.Background(), `UPDATE cicd_runs SET status=$2, error=$3, finished_at=now() WHERE id=$1`, runID, status, msg)
	m.log.Info("run finished", "run", runID, "status", status, "err", msg)
}

func (m *Module) runStages(ctx context.Context, runID int64, lw *logWriter) error {
	run, err := m.loadRun(ctx, runID)
	if err != nil {
		return err
	}
	pl, err := m.loadPipe(ctx, run.PipelineID)
	if err != nil {
		return err
	}
	a, err := m.loadApp(ctx, run.AppID)
	if err != nil {
		return err
	}
	var rt *runnerCtx
	needsRunner := false
	for _, s := range pl.Stages {
		needsRunner = needsRunner || s.Type == "build" || s.Type == "test" || s.Type == "script"
	}
	if needsRunner {
		cs, ns, _, err := m.targetClient(ctx, pl.RunnerTargetID)
		if err != nil {
			return err
		}
		rt = &runnerCtx{cs: cs, ns: ns, secret: fmt.Sprintf("cicd-run-%d", runID)}
		gitURL, err := m.cloneURL(a)
		if err != nil {
			return err
		}
		if err := rt.createSecret(ctx, map[string]string{"git_url": gitURL}, runID); err != nil {
			return fmt.Errorf("create run secret in %s: %w", ns, err)
		}
		defer func() {
			_ = rt.cs.CoreV1().Secrets(rt.ns).Delete(context.Background(), rt.secret, metav1.DeleteOptions{})
		}()
	}
	for i, s := range pl.Stages {
		if err := ctx.Err(); err != nil {
			return err
		}
		logf := lw.stage(i)
		logf("==> stage %s (%s)", s.Name, s.Type)
		m.setStage(runID, i, "running", "", "")
		var job string
		var serr error
		switch s.Type {
		case "build", "test", "script":
			job = fmt.Sprintf("cicd-%d-%d-%s", runID, i, s.Name)
			if len(job) > 63 {
				job = strings.TrimRight(job[:63], "-")
			}
			m.setStage(runID, i, "running", job, "")
			spec := stagePod(a, run, s, rt.secret)
			serr = runJob(ctx, rt.cs, rt.ns, job, runID, spec, s.TimeoutSec, logf)
		case "deploy":
			serr = m.deployStage(ctx, run, a, s, i, logf)
		case "jenkins":
			serr = m.jenkinsStage(ctx, run, a, s, logf)
		}
		if serr != nil {
			st := "failed"
			if ctx.Err() != nil {
				st = "cancelled"
			}
			if errors.Is(serr, errRejected) {
				st = "rejected"
			}
			m.setStage(runID, i, st, job, serr.Error())
			return fmt.Errorf("stage %s: %w", s.Name, serr)
		}
		m.setStage(runID, i, "succeeded", job, "")
	}
	return nil
}

// cloneURL embeds the app's Git credentials (decrypted) into an https URL.
// It is only ever stored in the per-run Kubernetes Secret.
func (m *Module) cloneURL(a *Application) (string, error) {
	if a.gitTokenEnc == "" || !strings.HasPrefix(a.RepoURL, "https://") {
		return a.RepoURL, nil
	}
	tok, err := m.d.Crypto.Decrypt(a.gitTokenEnc)
	if err != nil {
		return "", err
	}
	user := a.GitUsername
	if user == "" {
		user = "oauth2"
	}
	return "https://" + urlUserinfo(user, tok) + "@" + strings.TrimPrefix(a.RepoURL, "https://"), nil
}

type runnerCtx struct {
	cs     kubeIface
	ns     string
	secret string
}

func (r *runnerCtx) createSecret(ctx context.Context, data map[string]string, runID int64) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: r.secret, Labels: map[string]string{"app.kubernetes.io/managed-by": "matrix-cicd", "matrix.cicd/run": fmt.Sprint(runID)}},
		StringData: data,
	}
	_ = r.cs.CoreV1().Secrets(r.ns).Delete(ctx, r.secret, metav1.DeleteOptions{})
	_, err := r.cs.CoreV1().Secrets(r.ns).Create(ctx, sec, metav1.CreateOptions{})
	return err
}
