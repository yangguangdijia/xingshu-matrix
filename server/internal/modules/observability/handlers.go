package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// clientFor loads the datasource from :id (or ?datasource=) and builds a client.
func (m *Module) clientFor(c *gin.Context, idStr string, types ...string) (*client, bool) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, httpx.BadRequest("invalid datasource id"))
		return nil, false
	}
	ds, err := m.loadDS(c, rbac.PrincipalFrom(c), id, types...)
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	return newClient(ds), true
}

func respond(c *gin.Context, v any, err error) {
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, v)
}

// ------------------------------------------------------------------ Prometheus

func (m *Module) promQuery(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Param("id"), "prometheus"); ok {
		r, err := cl.PromInstant(c, c.Query("query"), c.Query("time"))
		respond(c, r, err)
	}
}

func (m *Module) promQueryRange(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Param("id"), "prometheus"); ok {
		r, err := cl.PromRange(c, c.Query("query"), c.Query("start"), c.Query("end"), c.Query("step"))
		respond(c, r, err)
	}
}

func (m *Module) promLabels(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Param("id"), "prometheus"); ok {
		r, err := cl.PromMeta(c, "/api/v1/labels", c.QueryArray("match[]"), c.Query("start"), c.Query("end"), false)
		respond(c, r, err)
	}
}

func (m *Module) promLabelValues(c *gin.Context) {
	name := c.Param("name")
	if !labelNameRe.MatchString(name) {
		httpx.Fail(c, httpx.Validation("invalid label name"))
		return
	}
	if cl, ok := m.clientFor(c, c.Param("id"), "prometheus"); ok {
		r, err := cl.PromMeta(c, "/api/v1/label/"+name+"/values", c.QueryArray("match[]"), c.Query("start"), c.Query("end"), false)
		respond(c, r, err)
	}
}

func (m *Module) promSeries(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Param("id"), "prometheus"); ok {
		r, err := cl.PromMeta(c, "/api/v1/series", c.QueryArray("match[]"), c.Query("start"), c.Query("end"), true)
		respond(c, r, err)
	}
}

func (m *Module) promRules(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Param("id"), "prometheus"); ok {
		q := map[string][]string{}
		if t := c.Query("type"); t == "alert" || t == "record" {
			q["type"] = []string{t}
		}
		r, err := cl.prom(c, "/api/v1/rules", q)
		respond(c, r, err)
	}
}

// ------------------------------------------------------------------------ Logs

func (m *Module) logsQuery(c *gin.Context) {
	var in LogQuery
	if !httpx.Bind(c, &in) {
		return
	}
	if cl, ok := m.clientFor(c, strconv.FormatInt(in.Datasource, 10), "loki", "elasticsearch"); ok {
		r, err := cl.QueryLogs(c, &in)
		respond(c, r, err)
	}
}

// ---------------------------------------------------------------------- Traces

func (m *Module) traceServices(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Query("datasource"), "jaeger", "tempo"); ok {
		r, err := cl.TraceServices(c)
		respond(c, r, err)
	}
}

func (m *Module) traceSearch(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Query("datasource"), "jaeger", "tempo"); ok {
		limit, _ := strconv.Atoi(c.Query("limit"))
		r, err := cl.SearchTraces(c, TraceSearch{Service: c.Query("service"), Operation: c.Query("operation"),
			Tags: c.Query("tags"), Query: c.Query("q"), MinDuration: c.Query("minDuration"), MaxDuration: c.Query("maxDuration"),
			From: c.Query("start"), To: c.Query("end"), Limit: limit})
		respond(c, r, err)
	}
}

func (m *Module) traceGet(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Query("datasource"), "jaeger", "tempo"); ok {
		r, err := cl.GetTrace(c, c.Param("traceId"))
		respond(c, r, err)
	}
}

// -------------------------------------------------------------------- Silences

func (m *Module) listSilences(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Query("datasource"), "alertmanager"); ok {
		r, err := cl.ListSilences(c, c.QueryArray("filter"))
		respond(c, r, err)
	}
}

func (m *Module) createSilence(c *gin.Context) {
	var in SilenceInput
	if !httpx.Bind(c, &in) {
		return
	}
	if cl, ok := m.clientFor(c, strconv.FormatInt(in.Datasource, 10), "alertmanager"); ok {
		id, err := cl.CreateSilence(c, &in, rbac.PrincipalFrom(c).Username)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "obs_silence", id)
		audit.SetDetail(c, in)
		httpx.Created(c, gin.H{"silenceID": id})
	}
}

func (m *Module) deleteSilence(c *gin.Context) {
	if cl, ok := m.clientFor(c, c.Query("datasource"), "alertmanager"); ok {
		err := cl.DeleteSilence(c, c.Param("sid"))
		audit.SetResource(c, "obs_silence", c.Param("sid"))
		respond(c, gin.H{"deleted": c.Param("sid")}, err)
	}
}

// ---------------------------------------------------------------------- Alerts

// amPayload is the Alertmanager webhook (version 4) body.
type amPayload struct {
	Version  string    `json:"version"`
	Receiver string    `json:"receiver"`
	Status   string    `json:"status"`
	Alerts   []amAlert `json:"alerts"`
}

type amAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

var labelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// labelFingerprint derives a stable fingerprint from sorted labels when
// the sender did not provide one.
func labelFingerprint(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k + "\xff" + l[k] + "\xff"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Alert is a stored alert.
type Alert struct {
	ID           int64             `json:"id"`
	DatasourceID int64             `json:"datasource_id"`
	Datasource   string            `json:"datasource"`
	Env          string            `json:"env"`
	Fingerprint  string            `json:"fingerprint"`
	Alertname    string            `json:"alertname"`
	Severity     string            `json:"severity"`
	Status       string            `json:"status"`
	HandleStatus string            `json:"handle_status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	GeneratorURL string            `json:"generator_url"`
	StartsAt     *time.Time        `json:"starts_at"`
	EndsAt       *time.Time        `json:"ends_at"`
	FireCount    int               `json:"fire_count"`
	AckedBy      string            `json:"acked_by"`
	AckedAt      *time.Time        `json:"acked_at"`
	ClosedBy     string            `json:"closed_by"`
	ClosedAt     *time.Time        `json:"closed_at"`
	CloseComment string            `json:"close_comment"`
	LastReceived time.Time         `json:"last_received_at"`
	CreatedAt    time.Time         `json:"created_at"`
}

func nilTime(t time.Time) *time.Time {
	if t.IsZero() || t.Year() < 1971 {
		return nil
	}
	return &t
}

func (m *Module) alertWebhook(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		token = strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	}
	if token == "" {
		httpx.Fail(c, httpx.Unauthorized("missing token"))
		return
	}
	var dsID int64
	var env, dsName string
	err := m.d.DB.QueryRow(c, `SELECT id,env,name FROM obs_datasources WHERE type='alertmanager' AND webhook_token_hash=$1`,
		hashToken(token)).Scan(&dsID, &env, &dsName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Fail(c, httpx.Unauthorized("invalid token"))
			return
		}
		httpx.Fail(c, err)
		return
	}
	var p amPayload
	if !httpx.Bind(c, &p) {
		return
	}
	var newlyFiring []Alert
	for _, a := range p.Alerts {
		if a.Status != "firing" && a.Status != "resolved" {
			continue
		}
		if a.Fingerprint == "" {
			a.Fingerprint = labelFingerprint(a.Labels)
		}
		lb, _ := json.Marshal(a.Labels)
		an, _ := json.Marshal(a.Annotations)
		var id int64
		var notify bool
		// Upsert by fingerprint. A firing alert that was resolved (or never seen)
		// reopens handling and is notified; repeats of a firing alert are not.
		var prev string
		err := m.d.DB.QueryRow(c, `WITH prev AS (
			  SELECT status FROM obs_alerts WHERE datasource_id=$1 AND fingerprint=$2
			), up AS (
			INSERT INTO obs_alerts (datasource_id,fingerprint,alertname,severity,status,labels,annotations,generator_url,starts_at,ends_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (datasource_id,fingerprint) DO UPDATE SET
			  alertname=EXCLUDED.alertname, severity=EXCLUDED.severity, labels=EXCLUDED.labels, annotations=EXCLUDED.annotations,
			  generator_url=EXCLUDED.generator_url, starts_at=EXCLUDED.starts_at, ends_at=EXCLUDED.ends_at,
			  fire_count = obs_alerts.fire_count + CASE WHEN obs_alerts.status='resolved' AND EXCLUDED.status='firing' THEN 1 ELSE 0 END,
			  handle_status = CASE WHEN obs_alerts.status='resolved' AND EXCLUDED.status='firing' THEN 'open' ELSE obs_alerts.handle_status END,
			  acked_by = CASE WHEN obs_alerts.status='resolved' AND EXCLUDED.status='firing' THEN '' ELSE obs_alerts.acked_by END,
			  status=EXCLUDED.status, last_received_at=now()
			RETURNING id)
			SELECT up.id, COALESCE((SELECT status FROM prev),'') FROM up`,
			dsID, a.Fingerprint, a.Labels["alertname"], a.Labels["severity"], a.Status, lb, an, a.GeneratorURL,
			nilTime(a.StartsAt), nilTime(a.EndsAt)).Scan(&id, &prev)
		// Notify on first sight of a firing alert or a resolved->firing transition.
		notify = a.Status == "firing" && prev != "firing"
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if notify {
			newlyFiring = append(newlyFiring, Alert{ID: id, DatasourceID: dsID, Datasource: dsName, Env: env, Fingerprint: a.Fingerprint,
				Alertname: a.Labels["alertname"], Severity: a.Labels["severity"], Status: "firing", Labels: a.Labels,
				Annotations: a.Annotations, GeneratorURL: a.GeneratorURL, StartsAt: nilTime(a.StartsAt)})
		}
	}
	if len(newlyFiring) > 0 {
		go m.notifyAlerts(context.Background(), newlyFiring)
	}
	httpx.OK(c, gin.H{"received": len(p.Alerts), "notified": len(newlyFiring)})
}

const alertCols = `a.id,a.datasource_id,d.name,d.env,a.fingerprint,a.alertname,a.severity,a.status,a.handle_status,a.labels,a.annotations,
	a.generator_url,a.starts_at,a.ends_at,a.fire_count,a.acked_by,a.acked_at,a.closed_by,a.closed_at,a.close_comment,a.last_received_at,a.created_at`

func scanAlert(row pgx.Row) (*Alert, error) {
	var a Alert
	var lb, an []byte
	if err := row.Scan(&a.ID, &a.DatasourceID, &a.Datasource, &a.Env, &a.Fingerprint, &a.Alertname, &a.Severity, &a.Status,
		&a.HandleStatus, &lb, &an, &a.GeneratorURL, &a.StartsAt, &a.EndsAt, &a.FireCount, &a.AckedBy, &a.AckedAt,
		&a.ClosedBy, &a.ClosedAt, &a.CloseComment, &a.LastReceived, &a.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("alert not found")
		}
		return nil, err
	}
	_ = json.Unmarshal(lb, &a.Labels)
	_ = json.Unmarshal(an, &a.Annotations)
	return &a, nil
}

// AlertFilter selects alerts.
type AlertFilter struct {
	Status, HandleStatus, Severity, Q string
	DatasourceID                      int64
}

func (m *Module) queryAlerts(ctx context.Context, p *rbac.Principal, f AlertFilter, pg httpx.Page) ([]*Alert, int64, error) {
	where := []string{"TRUE"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.Status != "" {
		add("a.status=?", f.Status)
	}
	if f.HandleStatus != "" {
		add("a.handle_status=?", f.HandleStatus)
	}
	if f.Severity != "" {
		add("a.severity=?", f.Severity)
	}
	if f.DatasourceID > 0 {
		add("a.datasource_id=?", f.DatasourceID)
	}
	if f.Q != "" {
		add("(a.alertname ILIKE ? OR a.annotations::text ILIKE ?)", "%"+f.Q+"%")
	}
	if p != nil && p.EnvScope != nil {
		all := false
		for _, e := range p.EnvScope {
			all = all || e == "*"
		}
		if !all {
			add("d.env = ANY(?)", p.EnvScope)
		}
	}
	w := strings.Join(where, " AND ")
	var total int64
	if err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM obs_alerts a JOIN obs_datasources d ON d.id=a.datasource_id WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pg.Limit(), pg.Offset())
	rows, err := m.d.DB.Query(ctx, `SELECT `+alertCols+` FROM obs_alerts a JOIN obs_datasources d ON d.id=a.datasource_id WHERE `+w+
		` ORDER BY (a.status='firing') DESC, a.last_received_at DESC LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

func (m *Module) listAlerts(c *gin.Context) {
	pg := httpx.ParsePage(c)
	dsID, _ := strconv.ParseInt(c.Query("datasource"), 10, 64)
	items, total, err := m.queryAlerts(c, rbac.PrincipalFrom(c), AlertFilter{Status: c.Query("status"),
		HandleStatus: c.Query("handle_status"), Severity: c.Query("severity"), Q: c.Query("q"), DatasourceID: dsID}, pg)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageResult(items, total, pg))
}

func (m *Module) loadAlert(c *gin.Context) (*Alert, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, false
	}
	a, err := scanAlert(m.d.DB.QueryRow(c, `SELECT `+alertCols+` FROM obs_alerts a JOIN obs_datasources d ON d.id=a.datasource_id WHERE a.id=$1`, id))
	if err == nil && !rbac.PrincipalFrom(c).CanEnv(a.Env) {
		err = httpx.Forbidden("no access to environment " + a.Env)
	}
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	return a, true
}

func (m *Module) getAlert(c *gin.Context) {
	if a, ok := m.loadAlert(c); ok {
		httpx.OK(c, a)
	}
}

func (m *Module) ackAlert(c *gin.Context) {
	a, ok := m.loadAlert(c)
	if !ok {
		return
	}
	if a.HandleStatus == "closed" {
		httpx.Fail(c, httpx.Conflict("alert already closed"))
		return
	}
	if a.HandleStatus == "acked" && a.AckedBy != "" {
		httpx.Fail(c, httpx.Conflict("alert already claimed by "+a.AckedBy))
		return
	}
	user := rbac.PrincipalFrom(c).Username
	tag, err := m.d.DB.Exec(c, `UPDATE obs_alerts SET handle_status='acked',acked_by=$2,acked_at=now() WHERE id=$1 AND handle_status='open'`, a.ID, user)
	if err == nil && tag.RowsAffected() == 0 {
		err = httpx.Conflict("alert state changed, reload")
	}
	audit.SetResource(c, "obs_alert", strconv.FormatInt(a.ID, 10))
	respond(c, gin.H{"id": a.ID, "handle_status": "acked", "acked_by": user}, err)
}

func (m *Module) closeAlert(c *gin.Context) {
	a, ok := m.loadAlert(c)
	if !ok {
		return
	}
	var in struct {
		Comment string `json:"comment"`
	}
	_ = c.ShouldBindJSON(&in)
	if a.HandleStatus == "closed" {
		httpx.Fail(c, httpx.Conflict("alert already closed"))
		return
	}
	user := rbac.PrincipalFrom(c).Username
	_, err := m.d.DB.Exec(c, `UPDATE obs_alerts SET handle_status='closed',closed_by=$2,closed_at=now(),close_comment=$3 WHERE id=$1`, a.ID, user, in.Comment)
	audit.SetResource(c, "obs_alert", strconv.FormatInt(a.ID, 10))
	respond(c, gin.H{"id": a.ID, "handle_status": "closed", "closed_by": user}, err)
}

// ---------------------------------------------------------------------- AI tools

func (m *Module) aiClient(ctx context.Context, id int64, types ...string) (*client, error) {
	p := rbac.FromContext(ctx)
	if p == nil {
		return nil, httpx.Unauthorized("no principal")
	}
	ds, err := m.loadDS(ctx, p, id, types...)
	if err != nil {
		return nil, err
	}
	return newClient(ds), nil
}

func (m *Module) registerAITools() {
	if m.d.AITools == nil {
		return
	}
	reg := func(t ai.Tool) {
		t.Module = "observability"
		if err := m.d.AITools.Register(t); err != nil && m.d.Logger != nil {
			m.d.Logger.Warn("register ai tool", "tool", t.Name, "err", err)
		}
	}
	reg(ai.Tool{
		Name:        "promql_query",
		Description: "Run a PromQL query against a Prometheus datasource. Without start/end it is an instant query; with start (e.g. now-1h) it is a range query.",
		Permission:  PermMetricsRead,
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"datasource":{"type":"integer","description":"Prometheus datasource id"},
			"query":{"type":"string","description":"PromQL expression"},
			"start":{"type":"string","description":"range start: RFC3339, unix seconds or now-1h"},
			"end":{"type":"string"},"step":{"type":"string","description":"e.g. 30s, 5m"}},
			"required":["datasource","query"]}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct {
				Datasource            int64
				Query, Start, End, Step string
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, err
			}
			cl, err := m.aiClient(ctx, a.Datasource, "prometheus")
			if err != nil {
				return nil, err
			}
			if a.Start != "" {
				return cl.PromRange(ctx, a.Query, a.Start, a.End, a.Step)
			}
			return cl.PromInstant(ctx, a.Query, "")
		},
	})
	reg(ai.Tool{
		Name:        "logs_query",
		Description: "Search logs in a Loki (LogQL) or Elasticsearch (Lucene / field:value AND ...) datasource. Returns normalized rows {ts, level, service, host, message}.",
		Permission:  PermLogsRead,
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"datasource":{"type":"integer"},"query":{"type":"string"},
			"from":{"type":"string","description":"default now-1h"},"to":{"type":"string"},
			"limit":{"type":"integer","description":"max rows, default 50"}},"required":["datasource","query"]}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var q LogQuery
			if err := json.Unmarshal(raw, &q); err != nil {
				return nil, err
			}
			if q.Limit <= 0 || q.Limit > 200 {
				q.Limit = 50
			}
			cl, err := m.aiClient(ctx, q.Datasource, "loki", "elasticsearch")
			if err != nil {
				return nil, err
			}
			r, err := cl.QueryLogs(ctx, &q)
			if err != nil {
				return nil, err
			}
			for i := range r.Rows { // keep tool output small
				r.Rows[i].Labels = nil
				r.Rows[i].Message = firstN(r.Rows[i].Message, 1000)
			}
			return r, nil
		},
	})
	reg(ai.Tool{
		Name:        "alerts_list",
		Description: "List alerts received from Alertmanager, newest firing first.",
		Permission:  PermAlertRead,
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"status":{"type":"string","enum":["firing","resolved"]},
			"handle_status":{"type":"string","enum":["open","acked","closed"]},
			"severity":{"type":"string"},"q":{"type":"string","description":"alertname/annotation substring"},
			"limit":{"type":"integer","description":"default 20"}}}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct {
				Status       string `json:"status"`
				HandleStatus string `json:"handle_status"`
				Severity     string `json:"severity"`
				Q            string `json:"q"`
				Limit        int    `json:"limit"`
			}
			_ = json.Unmarshal(raw, &a)
			if a.Limit <= 0 || a.Limit > 100 {
				a.Limit = 20
			}
			p := rbac.FromContext(ctx)
			if p == nil {
				return nil, httpx.Unauthorized("no principal")
			}
			items, total, err := m.queryAlerts(ctx, p, AlertFilter{Status: a.Status, HandleStatus: a.HandleStatus,
				Severity: a.Severity, Q: a.Q}, httpx.Page{Page: 1, Size: a.Limit})
			if err != nil {
				return nil, err
			}
			return gin.H{"total": total, "items": items}, nil
		},
	})
}
