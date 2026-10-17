// Package middleware manages middleware instances (Kafka, RabbitMQ, RocketMQ,
// MySQL, PostgreSQL, Redis): instance registry with encrypted credentials,
// connection tests, MQ administration and audited SQL / Redis consoles.
package middleware

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

const (
	TypeKafka    = "kafka"
	TypeRabbitMQ = "rabbitmq"
	TypeRocketMQ = "rocketmq"
	TypeMySQL    = "mysql"
	TypePostgres = "postgres"
	TypeRedis    = "redis"

	permRead    = "middleware:read"
	permManage  = "middleware:manage"
	permConsole = "middleware:console"
	permAudit   = "middleware:audit"

	approvalSQL    = "middleware.sql"
	approvalChange = "middleware.change"
)

var validTypes = map[string]bool{TypeKafka: true, TypeRabbitMQ: true, TypeRocketMQ: true, TypeMySQL: true, TypePostgres: true, TypeRedis: true}

// Module implements app.Module.
type Module struct{ d *app.Deps }

func New() *Module { return &Module{} }

func (m *Module) Name() string { return "middleware" }

func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }

func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{
		rbac.P("middleware", "middleware", "read", "查看中间件实例、Kafka/RabbitMQ/RocketMQ 监控信息"),
		rbac.P("middleware", "middleware", "manage", "管理中间件实例、Topic、队列"),
		rbac.P("middleware", "middleware", "console", "使用 SQL / Redis 控制台"),
		rbac.P("middleware", "middleware", "audit", "查看 SQL / Redis 控制台审计"),
	}
}

func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	g := r.Group("/middleware")
	g.GET("/instances", rbac.RequirePerm(permRead), m.listInstances)
	g.POST("/instances", rbac.RequirePerm(permManage), m.createInstance)
	g.POST("/instances/test", rbac.RequirePerm(permManage), m.testUnsaved)
	g.GET("/instances/:id", rbac.RequirePerm(permRead), m.getInstance)
	g.PUT("/instances/:id", rbac.RequirePerm(permManage), m.updateInstance)
	g.DELETE("/instances/:id", rbac.RequirePerm(permManage), m.deleteInstance)
	g.POST("/instances/:id/test", rbac.RequirePerm(permRead), m.testInstance)
	g.GET("/instances/:id/overview", rbac.RequirePerm(permRead), m.overview)

	k := g.Group("/instances/:id/kafka")
	k.GET("/brokers", rbac.RequirePerm(permRead), m.kafkaBrokers)
	k.GET("/topics", rbac.RequirePerm(permRead), m.kafkaTopics)
	k.GET("/topics/:topic", rbac.RequirePerm(permRead), m.kafkaTopic)
	k.POST("/topics", rbac.RequirePerm(permManage), m.kafkaCreateTopic)
	k.DELETE("/topics/:topic", rbac.RequirePerm(permManage), m.kafkaDeleteTopic)
	k.GET("/groups", rbac.RequirePerm(permRead), m.kafkaGroups)

	q := g.Group("/instances/:id/rabbitmq")
	q.GET("/overview", rbac.RequirePerm(permRead), m.rabbitGet("/api/overview"))
	q.GET("/nodes", rbac.RequirePerm(permRead), m.rabbitGet("/api/nodes"))
	q.GET("/queues", rbac.RequirePerm(permRead), m.rabbitGet("/api/queues"))
	q.GET("/exchanges", rbac.RequirePerm(permRead), m.rabbitGet("/api/exchanges"))
	q.GET("/connections", rbac.RequirePerm(permRead), m.rabbitGet("/api/connections"))
	q.POST("/queues", rbac.RequirePerm(permManage), m.rabbitCreateQueue)
	q.DELETE("/queues/:vhost/:name", rbac.RequirePerm(permManage), m.rabbitDeleteQueue)

	rq := g.Group("/instances/:id/rocketmq")
	rq.GET("/clusters", rbac.RequirePerm(permRead), m.rocketGet("/cluster/list.query"))
	rq.GET("/topics", rbac.RequirePerm(permRead), m.rocketGet("/topic/list.query"))

	g.POST("/instances/:id/sql", rbac.RequirePerm(permConsole), m.sqlConsole)
	g.POST("/instances/:id/redis", rbac.RequirePerm(permConsole), m.redisConsole)
	g.GET("/sql-audit", rbac.RequirePerm(permAudit), m.listAudit)

	m.registerApprovals(d)
	m.registerAITools(d)
}

// ---------------------------------------------------------------- model

// Auth is stored encrypted (JSON) in auth_enc.
type Auth struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	TLS           bool   `json:"tls,omitempty"`
	TLSSkipVerify bool   `json:"tls_skip_verify,omitempty"`
	// SASLMechanism (kafka only): PLAIN | SCRAM-SHA-256 | SCRAM-SHA-512; empty = no SASL.
	SASLMechanism string `json:"sasl_mechanism,omitempty"`
}

// Instance is a registered middleware endpoint.
type Instance struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Env         string            `json:"env"`
	Endpoints   []string          `json:"endpoints"`
	Auth        Auth              `json:"-"`
	Options     map[string]string `json:"options"`
	Description string            `json:"description"`
	CreatedBy   string            `json:"created_by"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// instanceView is the API representation (credentials masked).
type instanceView struct {
	Instance
	Auth authView `json:"auth"`
}

type authView struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	TLS           bool   `json:"tls"`
	TLSSkipVerify bool   `json:"tls_skip_verify"`
	SASLMechanism string `json:"sasl_mechanism"`
}

func view(i *Instance) instanceView {
	return instanceView{Instance: *i, Auth: authView{Username: i.Auth.Username, Password: crypto.Mask(i.Auth.Password),
		TLS: i.Auth.TLS, TLSSkipVerify: i.Auth.TLSSkipVerify, SASLMechanism: i.Auth.SASLMechanism}}
}

type instanceInput struct {
	Name        string            `json:"name" binding:"required,max=128"`
	Type        string            `json:"type" binding:"required"`
	Env         string            `json:"env" binding:"max=64"`
	Endpoints   []string          `json:"endpoints" binding:"required,min=1"`
	Auth        *Auth             `json:"auth"`
	Options     map[string]string `json:"options"`
	Description string            `json:"description"`
}

func (in *instanceInput) toInstance() (*Instance, error) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	if !validTypes[in.Type] {
		return nil, httpx.Validation("type must be one of kafka|rabbitmq|rocketmq|mysql|postgres|redis")
	}
	var eps []string
	for _, e := range in.Endpoints {
		if e = strings.TrimRight(strings.TrimSpace(e), "/"); e != "" {
			eps = append(eps, e)
		}
	}
	if len(eps) == 0 {
		return nil, httpx.Validation("endpoints required")
	}
	if (in.Type == TypeRabbitMQ || in.Type == TypeRocketMQ) && !strings.HasPrefix(eps[0], "http") {
		return nil, httpx.Validation("rabbitmq/rocketmq endpoints must be http(s) URLs (management API / rocketmq-dashboard)")
	}
	inst := &Instance{Name: strings.TrimSpace(in.Name), Type: in.Type, Env: strings.TrimSpace(in.Env), Endpoints: eps,
		Options: in.Options, Description: in.Description}
	if in.Auth != nil {
		inst.Auth = *in.Auth
	}
	if inst.Options == nil {
		inst.Options = map[string]string{}
	}
	return inst, nil
}

// ---------------------------------------------------------------- repo

const instCols = `id,name,type,env,endpoints,auth_enc,options,description,created_by,created_at,updated_at`

func (m *Module) scanInstance(row pgx.Row) (*Instance, error) {
	var i Instance
	var eps, opts []byte
	var enc string
	if err := row.Scan(&i.ID, &i.Name, &i.Type, &i.Env, &eps, &enc, &opts, &i.Description, &i.CreatedBy, &i.CreatedAt, &i.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(eps, &i.Endpoints)
	_ = json.Unmarshal(opts, &i.Options)
	if i.Options == nil {
		i.Options = map[string]string{}
	}
	if enc != "" {
		plain, err := m.d.Crypto.Decrypt(enc)
		if err != nil {
			return nil, fmt.Errorf("decrypt instance auth: %w", err)
		}
		_ = json.Unmarshal([]byte(plain), &i.Auth)
	}
	return &i, nil
}

func (m *Module) encAuth(a Auth) (string, error) {
	if a == (Auth{}) {
		return "", nil
	}
	b, _ := json.Marshal(a)
	return m.d.Crypto.Encrypt(string(b))
}

func (m *Module) loadInstance(ctx context.Context, id int64) (*Instance, error) {
	inst, err := m.scanInstance(m.d.DB.QueryRow(ctx, `SELECT `+instCols+` FROM middleware_instances WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("instance not found")
	}
	return inst, err
}

// instanceFor loads :id, checks env scope and (optionally) the expected type.
func (m *Module) instanceFor(c *gin.Context, types ...string) (*Instance, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, false
	}
	inst, err := m.loadInstance(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	if !rbac.PrincipalFrom(c).CanEnv(inst.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+inst.Env))
		return nil, false
	}
	if len(types) > 0 && !contains(types, inst.Type) {
		httpx.Fail(c, httpx.BadRequest(fmt.Sprintf("instance %d is %s, expected %s", inst.ID, inst.Type, strings.Join(types, "|"))))
		return nil, false
	}
	audit.SetResource(c, "middleware_instance", strconv.FormatInt(inst.ID, 10))
	return inst, true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- instance handlers

func (m *Module) listInstances(c *gin.Context) {
	p := httpx.ParsePage(c)
	pr := rbac.PrincipalFrom(c)
	where, args := []string{"1=1"}, []any{}
	if t := c.Query("type"); t != "" {
		args = append(args, t)
		where = append(where, fmt.Sprintf("type=$%d", len(args)))
	}
	if e := c.Query("env"); e != "" {
		args = append(args, e)
		where = append(where, fmt.Sprintf("env=$%d", len(args)))
	}
	if kw := c.Query("keyword"); kw != "" {
		args = append(args, "%"+kw+"%")
		where = append(where, fmt.Sprintf("name ILIKE $%d", len(args)))
	}
	if pr.EnvScope != nil && !contains(pr.EnvScope, "*") {
		args = append(args, pr.EnvScope)
		where = append(where, fmt.Sprintf("env = ANY($%d)", len(args)))
	}
	w := strings.Join(where, " AND ")
	ctx := c.Request.Context()
	var total int64
	if err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM middleware_instances WHERE `+w, args...).Scan(&total); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	args = append(args, p.Limit(), p.Offset())
	rows, err := m.d.DB.Query(ctx, fmt.Sprintf(`SELECT %s FROM middleware_instances WHERE %s ORDER BY id DESC LIMIT $%d OFFSET $%d`, instCols, w, len(args)-1, len(args)), args...)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	defer rows.Close()
	var items []instanceView
	for rows.Next() {
		inst, err := m.scanInstance(rows)
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		items = append(items, view(inst))
	}
	httpx.OK(c, httpx.NewPageResult(items, total, p))
}

func (m *Module) getInstance(c *gin.Context) {
	if inst, ok := m.instanceFor(c); ok {
		httpx.OK(c, view(inst))
	}
}

func (m *Module) createInstance(c *gin.Context) {
	var in instanceInput
	if !httpx.Bind(c, &in) {
		return
	}
	inst, err := in.toInstance()
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	pr := rbac.PrincipalFrom(c)
	if !pr.CanEnv(inst.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+inst.Env))
		return
	}
	enc, err := m.encAuth(inst.Auth)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	eps, _ := json.Marshal(inst.Endpoints)
	opts, _ := json.Marshal(inst.Options)
	err = m.d.DB.QueryRow(c.Request.Context(), `INSERT INTO middleware_instances(name,type,env,endpoints,auth_enc,options,description,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,created_at,updated_at`,
		inst.Name, inst.Type, inst.Env, eps, enc, opts, inst.Description, pr.Username).Scan(&inst.ID, &inst.CreatedAt, &inst.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "23505") {
			httpx.Fail(c, httpx.Conflict("instance name already exists"))
			return
		}
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	inst.CreatedBy = pr.Username
	audit.SetResource(c, "middleware_instance", strconv.FormatInt(inst.ID, 10))
	httpx.Created(c, view(inst))
}

func (m *Module) updateInstance(c *gin.Context) {
	old, ok := m.instanceFor(c)
	if !ok {
		return
	}
	var in instanceInput
	if !httpx.Bind(c, &in) {
		return
	}
	inst, err := in.toInstance()
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if !rbac.PrincipalFrom(c).CanEnv(inst.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+inst.Env))
		return
	}
	if in.Auth == nil {
		inst.Auth = old.Auth // keep stored credentials
	} else if in.Auth.Password == "" || strings.Contains(in.Auth.Password, "****") {
		inst.Auth.Password = old.Auth.Password // masked/empty password = unchanged
	}
	enc, err := m.encAuth(inst.Auth)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	eps, _ := json.Marshal(inst.Endpoints)
	opts, _ := json.Marshal(inst.Options)
	inst.ID, inst.CreatedBy, inst.CreatedAt = old.ID, old.CreatedBy, old.CreatedAt
	err = m.d.DB.QueryRow(c.Request.Context(), `UPDATE middleware_instances SET name=$1,type=$2,env=$3,endpoints=$4,auth_enc=$5,options=$6,description=$7,updated_at=now()
		WHERE id=$8 RETURNING updated_at`, inst.Name, inst.Type, inst.Env, eps, enc, opts, inst.Description, old.ID).Scan(&inst.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "23505") {
			httpx.Fail(c, httpx.Conflict("instance name already exists"))
			return
		}
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, view(inst))
}

func (m *Module) deleteInstance(c *gin.Context) {
	inst, ok := m.instanceFor(c)
	if !ok {
		return
	}
	if _, err := m.d.DB.Exec(c.Request.Context(), `DELETE FROM middleware_instances WHERE id=$1`, inst.ID); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, gin.H{"id": inst.ID})
}

func (m *Module) testInstance(c *gin.Context) {
	if inst, ok := m.instanceFor(c); ok {
		httpx.OK(c, testConnection(c.Request.Context(), inst))
	}
}

func (m *Module) testUnsaved(c *gin.Context) {
	var in instanceInput
	if !httpx.Bind(c, &in) {
		return
	}
	inst, err := in.toInstance()
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, testConnection(c.Request.Context(), inst))
}

// TestResult is returned by connection tests.
type TestResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Version   string `json:"version,omitempty"`
	Message   string `json:"message"`
}

func testConnection(ctx context.Context, inst *Instance) TestResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	ver, err := ping(ctx, inst)
	res := TestResult{OK: err == nil, LatencyMS: time.Since(start).Milliseconds(), Version: ver, Message: "连接成功"}
	if err != nil {
		res.Message = err.Error()
	}
	return res
}

func ping(ctx context.Context, inst *Instance) (string, error) {
	switch inst.Type {
	case TypeKafka:
		adm, err := kafkaAdmin(inst)
		if err != nil {
			return "", err
		}
		defer adm.Close()
		bs, err := adm.ListBrokers(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d brokers", len(bs)), nil
	case TypeRabbitMQ:
		var ov struct {
			RabbitMQVersion string `json:"rabbitmq_version"`
		}
		if err := rabbitDo(ctx, inst, "GET", "/api/overview", nil, &ov); err != nil {
			return "", err
		}
		return "RabbitMQ " + ov.RabbitMQVersion, nil
	case TypeRocketMQ:
		_, err := rocketGetData(ctx, inst, "/cluster/list.query")
		return "rocketmq-dashboard", err
	case TypeMySQL, TypePostgres:
		return sqlPing(ctx, inst)
	case TypeRedis:
		cl := redisClient(inst, -1)
		defer cl.Close()
		info, err := cl.Info(ctx, "server").Result()
		if err != nil {
			return "", err
		}
		for _, l := range strings.Split(info, "\n") {
			if strings.HasPrefix(l, "redis_version:") {
				return "Redis " + strings.TrimSpace(strings.TrimPrefix(l, "redis_version:")), nil
			}
		}
		return "Redis", nil
	}
	return "", fmt.Errorf("unsupported type %s", inst.Type)
}

// overview returns an MQ summary (also used by AI tool mq_overview).
func (m *Module) overview(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeKafka, TypeRabbitMQ, TypeRocketMQ)
	if !ok {
		return
	}
	v, err := mqOverview(c.Request.Context(), inst)
	if err != nil {
		httpx.Fail(c, httpx.NewError(502, 50200, "upstream error: "+err.Error()))
		return
	}
	httpx.OK(c, v)
}

func upstream(c *gin.Context, err error) {
	var he *httpx.Error
	if errors.As(err, &he) {
		httpx.Fail(c, err)
		return
	}
	httpx.Fail(c, httpx.NewError(502, 50200, "upstream error: "+err.Error()))
}

// ---------------------------------------------------------------- gated changes

// change is a gated MQ write (payload of approval type middleware.change).
type change struct {
	Op                string             `json:"op"` // kafka.create_topic | kafka.delete_topic | rabbitmq.create_queue | rabbitmq.delete_queue
	InstanceID        int64              `json:"instance_id"`
	Topic             string             `json:"topic,omitempty"`
	Partitions        int32              `json:"partitions,omitempty"`
	ReplicationFactor int16              `json:"replication_factor,omitempty"`
	Configs           map[string]*string `json:"configs,omitempty"`
	VHost             string             `json:"vhost,omitempty"`
	Queue             string             `json:"queue,omitempty"`
	Durable           bool               `json:"durable,omitempty"`
	AutoDelete        bool               `json:"auto_delete,omitempty"`
	Arguments         map[string]any     `json:"arguments,omitempty"`
}

// runChange gates ch on the instance env and executes it immediately when no approval is required.
func (m *Module) runChange(c *gin.Context, inst *Instance, ch change, title string) {
	audit.SetDetail(c, ch)
	t, pending, err := m.d.Approval.Gate(c.Request.Context(), rbac.PrincipalFrom(c), approval.CreateInput{
		Type: approvalChange, Title: fmt.Sprintf("[%s] %s", inst.Name, title), Env: inst.Env, Payload: ch})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if pending {
		approval.RespondPending(c, t)
		return
	}
	res, err := m.execChange(c.Request.Context(), inst, ch)
	if err != nil {
		upstream(c, err)
		return
	}
	httpx.OK(c, res)
}

func (m *Module) execChange(ctx context.Context, inst *Instance, ch change) (any, error) {
	switch ch.Op {
	case "kafka.create_topic":
		return kafkaCreateTopic(ctx, inst, ch)
	case "kafka.delete_topic":
		return kafkaDeleteTopic(ctx, inst, ch.Topic)
	case "rabbitmq.create_queue":
		body := map[string]any{"durable": ch.Durable, "auto_delete": ch.AutoDelete, "arguments": ch.Arguments}
		if ch.Arguments == nil {
			body["arguments"] = map[string]any{}
		}
		return gin.H{"vhost": ch.VHost, "queue": ch.Queue, "created": true},
			rabbitDo(ctx, inst, "PUT", "/api/queues/"+escVhost(ch.VHost)+"/"+escVhost(ch.Queue), body, nil)
	case "rabbitmq.delete_queue":
		return gin.H{"vhost": ch.VHost, "queue": ch.Queue, "deleted": true},
			rabbitDo(ctx, inst, "DELETE", "/api/queues/"+escVhost(ch.VHost)+"/"+escVhost(ch.Queue), nil, nil)
	}
	return nil, fmt.Errorf("unknown op %q", ch.Op)
}

func (m *Module) registerApprovals(d *app.Deps) {
	if d.Approval == nil {
		return
	}
	specs := []approval.TypeSpec{
		{Type: approvalSQL, Module: "middleware", Description: "生产库高危 SQL（DROP/TRUNCATE/ALTER/无 WHERE 的 DELETE/UPDATE 等）执行审批",
			CreatePermission: permConsole, Timeout: 5 * time.Minute, Handler: approval.HandlerFunc(m.onSQLApproved)},
		{Type: approvalChange, Module: "middleware", Description: "中间件变更（Kafka Topic / RabbitMQ 队列创建删除）审批",
			CreatePermission: permManage, Timeout: 2 * time.Minute, Handler: approval.HandlerFunc(func(ctx context.Context, t *approval.Ticket) (any, error) {
				var ch change
				if err := t.DecodePayload(&ch); err != nil {
					return nil, err
				}
				inst, err := m.loadInstance(ctx, ch.InstanceID)
				if err != nil {
					return nil, err
				}
				return m.execChange(ctx, inst, ch)
			})},
	}
	for _, s := range specs {
		if err := d.Approval.Register(s); err != nil && d.Logger != nil {
			d.Logger.Warn("register approval type", "type", s.Type, "err", err)
		}
	}
}

// ---------------------------------------------------------------- AI tools

type toolArgs struct {
	InstanceID int64  `json:"instance_id"`
	Group      string `json:"group"`
}

func (m *Module) toolInstance(ctx context.Context, raw json.RawMessage, types ...string) (*Instance, toolArgs, error) {
	var a toolArgs
	if err := json.Unmarshal(raw, &a); err != nil || a.InstanceID <= 0 {
		return nil, a, errors.New("instance_id is required")
	}
	inst, err := m.loadInstance(ctx, a.InstanceID)
	if err != nil {
		return nil, a, err
	}
	if p := rbac.FromContext(ctx); p != nil && !p.CanEnv(inst.Env) {
		return nil, a, errors.New("environment not in your scope: " + inst.Env)
	}
	if !contains(types, inst.Type) {
		return nil, a, fmt.Errorf("instance %d is %s, expected %s", inst.ID, inst.Type, strings.Join(types, "|"))
	}
	return inst, a, nil
}

func (m *Module) registerAITools(d *app.Deps) {
	if d.AITools == nil {
		return
	}
	tools := []ai.Tool{
		{Name: "kafka_consumer_lag", Module: "middleware", Permission: permRead,
			Description: "查询 Kafka 实例的消费组堆积（lag）：每个消费组的状态、成员数、总 lag 以及按 Topic 的 lag。可指定 group 只查一个消费组。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"instance_id":{"type":"integer","description":"中间件实例 ID（type=kafka）"},"group":{"type":"string","description":"消费组名，可选"}},"required":["instance_id"]}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				inst, a, err := m.toolInstance(ctx, raw, TypeKafka)
				if err != nil {
					return nil, err
				}
				var groups []string
				if a.Group != "" {
					groups = []string{a.Group}
				}
				return kafkaGroupLags(ctx, inst, groups, false)
			}},
		{Name: "mq_overview", Module: "middleware", Permission: permRead,
			Description: "获取消息队列实例（Kafka / RabbitMQ / RocketMQ）概览：Broker/节点、Topic/队列数量、消息速率与堆积等。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"instance_id":{"type":"integer","description":"中间件实例 ID（kafka|rabbitmq|rocketmq）"}},"required":["instance_id"]}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				inst, _, err := m.toolInstance(ctx, raw, TypeKafka, TypeRabbitMQ, TypeRocketMQ)
				if err != nil {
					return nil, err
				}
				return mqOverview(ctx, inst)
			}},
	}
	for _, t := range tools {
		if err := d.AITools.Register(t); err != nil && d.Logger != nil {
			d.Logger.Warn("register ai tool", "tool", t.Name, "err", err)
		}
	}
}

func mqOverview(ctx context.Context, inst *Instance) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	switch inst.Type {
	case TypeKafka:
		return kafkaOverview(ctx, inst)
	case TypeRabbitMQ:
		return rabbitOverview(ctx, inst)
	case TypeRocketMQ:
		data, err := rocketGetData(ctx, inst, "/cluster/list.query")
		if err != nil {
			return nil, err
		}
		return gin.H{"type": TypeRocketMQ, "instance": inst.Name, "cluster": data}, nil
	}
	return nil, fmt.Errorf("instance type %s has no MQ overview", inst.Type)
}
