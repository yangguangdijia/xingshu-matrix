// Package observability implements data-source management and proxies to
// Prometheus, Loki, Elasticsearch, Alertmanager, Jaeger and Tempo, plus an
// Alertmanager webhook receiver, alert handling and notification channels.
package observability

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

// Permission codes.
const (
	PermDSRead      = "datasource:read"
	PermDSManage    = "datasource:manage"
	PermMetricsRead = "metrics:read"
	PermLogsRead    = "logs:read"
	PermAlertRead   = "alert:read"
	PermAlertHandle = "alert:handle"
	PermSilence     = "alert:silence"
	PermTraceRead   = "trace:read"
	PermNotify      = "notify:manage"
)

// Module is the observability module.
type Module struct {
	d *app.Deps
}

// New returns the observability module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "observability" }

func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }

func (m *Module) Permissions() []rbac.Permission {
	const mod = "observability"
	return []rbac.Permission{
		rbac.P(mod, "datasource", "read", "查看可观测数据源"),
		rbac.P(mod, "datasource", "manage", "管理可观测数据源"),
		rbac.P(mod, "metrics", "read", "查询指标 (PromQL)"),
		rbac.P(mod, "logs", "read", "查询日志 (Loki/ES)"),
		rbac.P(mod, "alert", "read", "查看告警/告警规则/静默"),
		rbac.P(mod, "alert", "handle", "认领/关闭告警"),
		rbac.P(mod, "alert", "silence", "创建/删除 Alertmanager 静默"),
		rbac.P(mod, "trace", "read", "查询链路追踪"),
		rbac.P(mod, "notify", "manage", "管理告警通知渠道"),
	}
}

func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	g := r.Group("/observability")

	ds := g.Group("/datasources")
	ds.GET("", rbac.RequirePerm(PermDSRead), m.listDS)
	ds.POST("", rbac.RequirePerm(PermDSManage), m.createDS)
	ds.GET("/:id", rbac.RequirePerm(PermDSRead), m.getDS)
	ds.PUT("/:id", rbac.RequirePerm(PermDSManage), m.updateDS)
	ds.DELETE("/:id", rbac.RequirePerm(PermDSManage), m.deleteDS)
	ds.POST("/:id/test", rbac.RequirePerm(PermDSRead), m.testDS)
	ds.POST("/:id/webhook-token", rbac.RequirePerm(PermDSManage), m.rotateToken)

	pr := g.Group("/prometheus/:id")
	pr.GET("/query", rbac.RequirePerm(PermMetricsRead), m.promQuery)
	pr.GET("/query_range", rbac.RequirePerm(PermMetricsRead), m.promQueryRange)
	pr.GET("/labels", rbac.RequirePerm(PermMetricsRead), m.promLabels)
	pr.GET("/label/:name/values", rbac.RequirePerm(PermMetricsRead), m.promLabelValues)
	pr.GET("/series", rbac.RequirePerm(PermMetricsRead), m.promSeries)
	pr.GET("/rules", rbac.RequirePerm(PermAlertRead), m.promRules)

	g.POST("/logs/query", rbac.RequirePerm(PermLogsRead), m.logsQuery)

	al := g.Group("/alerts")
	al.GET("", rbac.RequirePerm(PermAlertRead), m.listAlerts)
	al.GET("/:id", rbac.RequirePerm(PermAlertRead), m.getAlert)
	al.POST("/:id/ack", rbac.RequirePerm(PermAlertHandle), m.ackAlert)
	al.POST("/:id/close", rbac.RequirePerm(PermAlertHandle), m.closeAlert)

	si := g.Group("/silences")
	si.GET("", rbac.RequirePerm(PermAlertRead), m.listSilences)
	si.POST("", rbac.RequirePerm(PermSilence), m.createSilence)
	si.DELETE("/:sid", rbac.RequirePerm(PermSilence), m.deleteSilence)

	tr := g.Group("/traces")
	tr.GET("/services", rbac.RequirePerm(PermTraceRead), m.traceServices)
	tr.GET("/search", rbac.RequirePerm(PermTraceRead), m.traceSearch)
	tr.GET("/:traceId", rbac.RequirePerm(PermTraceRead), m.traceGet)

	ch := g.Group("/channels")
	ch.GET("", rbac.RequirePerm(PermNotify), m.listChannels)
	ch.POST("", rbac.RequirePerm(PermNotify), m.createChannel)
	ch.GET("/:id", rbac.RequirePerm(PermNotify), m.getChannel)
	ch.PUT("/:id", rbac.RequirePerm(PermNotify), m.updateChannel)
	ch.DELETE("/:id", rbac.RequirePerm(PermNotify), m.deleteChannel)
	ch.POST("/:id/test", rbac.RequirePerm(PermNotify), m.testChannel)

	// Unauthenticated Alertmanager receiver, authenticated by ?token=.
	if d.Public != nil {
		d.Public.POST("/alerts/webhook", m.alertWebhook)
	}

	m.registerAITools()
}

// ---------------------------------------------------------------- datasources

var dsTypes = map[string]bool{"prometheus": true, "loki": true, "elasticsearch": true, "alertmanager": true, "jaeger": true, "tempo": true}

// DataSource is the stored data-source row (secret decrypted only in memory).
type DataSource struct {
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	URL           string         `json:"url"`
	Env           string         `json:"env"`
	AuthType      string         `json:"auth_type"`
	Username      string         `json:"username"`
	Secret        string         `json:"secret,omitempty"` // masked in responses
	TLSSkipVerify bool           `json:"tls_skip_verify"`
	TimeoutSec    int            `json:"timeout_sec"`
	Options       map[string]any `json:"options"`
	HasWebhook    bool           `json:"has_webhook_token"`
	CreatedBy     string         `json:"created_by"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`

	secretPlain string
}

func (ds *DataSource) opt(key string) string {
	if v, ok := ds.Options[key].(string); ok {
		return v
	}
	return ""
}

type dsInput struct {
	Name          string         `json:"name" binding:"required,max=64"`
	Type          string         `json:"type" binding:"required"`
	URL           string         `json:"url" binding:"required"`
	Env           string         `json:"env"`
	AuthType      string         `json:"auth_type"`
	Username      string         `json:"username"`
	Secret        *string        `json:"secret"` // password or bearer token; nil = keep
	TLSSkipVerify bool           `json:"tls_skip_verify"`
	TimeoutSec    int            `json:"timeout_sec"`
	Options       map[string]any `json:"options"` // es: index,time_field,message_field; loki: org_id
}

func (in *dsInput) validate() error {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	if !dsTypes[in.Type] {
		return httpx.Validation("type must be one of prometheus|loki|elasticsearch|alertmanager|jaeger|tempo")
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return httpx.Validation("url must be an absolute http(s) URL")
	}
	in.URL = strings.TrimRight(u.String(), "/")
	if in.AuthType == "" {
		in.AuthType = "none"
	}
	if in.AuthType != "none" && in.AuthType != "basic" && in.AuthType != "bearer" {
		return httpx.Validation("auth_type must be none|basic|bearer")
	}
	if in.AuthType == "basic" && in.Username == "" {
		return httpx.Validation("username required for basic auth")
	}
	if in.TimeoutSec <= 0 || in.TimeoutSec > 300 {
		in.TimeoutSec = 30
	}
	if in.Options == nil {
		in.Options = map[string]any{}
	}
	return nil
}

const dsCols = `id,name,type,url,env,auth_type,username,secret_enc,tls_skip_verify,timeout_sec,options,webhook_token_hash<>'',created_by,created_at,updated_at`

func (m *Module) scanDS(row pgx.Row) (*DataSource, error) {
	var ds DataSource
	var enc string
	var opts []byte
	if err := row.Scan(&ds.ID, &ds.Name, &ds.Type, &ds.URL, &ds.Env, &ds.AuthType, &ds.Username, &enc,
		&ds.TLSSkipVerify, &ds.TimeoutSec, &opts, &ds.HasWebhook, &ds.CreatedBy, &ds.CreatedAt, &ds.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("datasource not found")
		}
		return nil, err
	}
	_ = json.Unmarshal(opts, &ds.Options)
	if ds.Options == nil {
		ds.Options = map[string]any{}
	}
	if enc != "" {
		plain, err := m.d.Crypto.Decrypt(enc)
		if err != nil {
			return nil, httpx.Internal(err)
		}
		ds.secretPlain = plain
		ds.Secret = crypto.Mask(plain)
	}
	return &ds, nil
}

// loadDS loads a datasource, enforcing the caller's env scope and an optional type set.
func (m *Module) loadDS(ctx context.Context, p *rbac.Principal, id int64, types ...string) (*DataSource, error) {
	ds, err := m.scanDS(m.d.DB.QueryRow(ctx, `SELECT `+dsCols+` FROM obs_datasources WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	if p != nil && !p.CanEnv(ds.Env) {
		return nil, httpx.Forbidden("no access to environment " + ds.Env)
	}
	if len(types) > 0 {
		ok := false
		for _, t := range types {
			ok = ok || ds.Type == t
		}
		if !ok {
			return nil, httpx.BadRequest("datasource type " + ds.Type + " not supported here (want " + strings.Join(types, "|") + ")")
		}
	}
	return ds, nil
}

func dsIDParam(c *gin.Context) (int64, bool) { return httpx.ParamID(c, "id") }

func (m *Module) listDS(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	q := `SELECT ` + dsCols + ` FROM obs_datasources WHERE ($1='' OR type=$1) ORDER BY id`
	rows, err := m.d.DB.Query(c, q, c.Query("type"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []*DataSource{}
	for rows.Next() {
		ds, err := m.scanDS(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if p.CanEnv(ds.Env) {
			out = append(out, ds)
		}
	}
	httpx.OK(c, out)
}

func (m *Module) getDS(c *gin.Context) {
	id, ok := dsIDParam(c)
	if !ok {
		return
	}
	ds, err := m.loadDS(c, rbac.PrincipalFrom(c), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, ds)
}

func newToken() (plain, hash string) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	plain = hex.EncodeToString(b)
	return plain, hashToken(plain)
}

func hashToken(t string) string { s := sha256.Sum256([]byte(t)); return hex.EncodeToString(s[:]) }

func (m *Module) createDS(c *gin.Context) {
	var in dsInput
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.validate(); err != nil {
		httpx.Fail(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	if !p.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("no access to environment "+in.Env))
		return
	}
	enc := ""
	if in.Secret != nil && *in.Secret != "" {
		var err error
		if enc, err = m.d.Crypto.Encrypt(*in.Secret); err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
	}
	token, hash := "", ""
	if in.Type == "alertmanager" {
		token, hash = newToken()
	}
	opts, _ := json.Marshal(in.Options)
	var id int64
	err := m.d.DB.QueryRow(c, `INSERT INTO obs_datasources (name,type,url,env,auth_type,username,secret_enc,tls_skip_verify,timeout_sec,options,webhook_token_hash,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		in.Name, in.Type, in.URL, in.Env, in.AuthType, in.Username, enc, in.TLSSkipVerify, in.TimeoutSec, opts, hash, p.Username).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			httpx.Fail(c, httpx.Conflict("datasource name already exists"))
			return
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_datasource", strconv.FormatInt(id, 10))
	ds, err := m.loadDS(c, nil, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	resp := gin.H{"datasource": ds}
	if token != "" {
		resp["webhook_token"] = token // shown once
		resp["webhook_path"] = "/api/v1/alerts/webhook?token=" + token
	}
	httpx.Created(c, resp)
}

func (m *Module) updateDS(c *gin.Context) {
	id, ok := dsIDParam(c)
	if !ok {
		return
	}
	p := rbac.PrincipalFrom(c)
	cur, err := m.loadDS(c, p, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var in dsInput
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.validate(); err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.Type != cur.Type {
		httpx.Fail(c, httpx.Validation("type cannot be changed"))
		return
	}
	if !p.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("no access to environment "+in.Env))
		return
	}
	var encArg any // nil keeps the current secret
	if in.Secret != nil {
		enc := ""
		if *in.Secret != "" {
			if enc, err = m.d.Crypto.Encrypt(*in.Secret); err != nil {
				httpx.Fail(c, httpx.Internal(err))
				return
			}
		}
		encArg = enc
	}
	opts, _ := json.Marshal(in.Options)
	_, err = m.d.DB.Exec(c, `UPDATE obs_datasources SET name=$2,url=$3,env=$4,auth_type=$5,username=$6,secret_enc=COALESCE($7,secret_enc),
		tls_skip_verify=$8,timeout_sec=$9,options=$10,updated_at=now() WHERE id=$1`,
		id, in.Name, in.URL, in.Env, in.AuthType, in.Username, encArg, in.TLSSkipVerify, in.TimeoutSec, opts)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			httpx.Fail(c, httpx.Conflict("datasource name already exists"))
			return
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_datasource", strconv.FormatInt(id, 10))
	ds, err := m.loadDS(c, nil, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, ds)
}

func (m *Module) deleteDS(c *gin.Context) {
	id, ok := dsIDParam(c)
	if !ok {
		return
	}
	if _, err := m.loadDS(c, rbac.PrincipalFrom(c), id); err != nil {
		httpx.Fail(c, err)
		return
	}
	if _, err := m.d.DB.Exec(c, `DELETE FROM obs_datasources WHERE id=$1`, id); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_datasource", strconv.FormatInt(id, 10))
	httpx.OK(c, gin.H{"deleted": id})
}

func (m *Module) rotateToken(c *gin.Context) {
	id, ok := dsIDParam(c)
	if !ok {
		return
	}
	if _, err := m.loadDS(c, rbac.PrincipalFrom(c), id, "alertmanager"); err != nil {
		httpx.Fail(c, err)
		return
	}
	token, hash := newToken()
	if _, err := m.d.DB.Exec(c, `UPDATE obs_datasources SET webhook_token_hash=$2,updated_at=now() WHERE id=$1`, id, hash); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_datasource", strconv.FormatInt(id, 10))
	httpx.OK(c, gin.H{"webhook_token": token, "webhook_path": "/api/v1/alerts/webhook?token=" + token})
}

func (m *Module) testDS(c *gin.Context) {
	id, ok := dsIDParam(c)
	if !ok {
		return
	}
	ds, err := m.loadDS(c, rbac.PrincipalFrom(c), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	start := time.Now()
	info, err := newClient(ds).Test(c)
	res := gin.H{"ok": err == nil, "latency_ms": time.Since(start).Milliseconds(), "info": info}
	if err != nil {
		res["error"] = err.Error()
	}
	httpx.OK(c, res)
}
