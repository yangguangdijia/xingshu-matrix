package middleware

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	"github.com/redis/go-redis/v9"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	maxRows           = 500
	defaultTimeoutSec = 30
	maxTimeoutSec     = 300

	RiskRead      = "read"
	RiskWrite     = "write"
	RiskDangerous = "dangerous"
)

// isProdEnv reports whether an env tag denotes production.
func isProdEnv(env string) bool {
	e := strings.ToLower(strings.TrimSpace(env))
	return e == "prod" || e == "prd" || e == "production" || strings.HasPrefix(e, "prod-") || strings.HasSuffix(e, "-prod")
}

// ---------------------------------------------------------------- SQL risk analysis

// SQLRisk is the result of analysing one statement.
type SQLRisk struct {
	Level   string `json:"level"`   // read | write | dangerous
	Keyword string `json:"keyword"` // leading keyword (SELECT, DELETE, ...)
	Reason  string `json:"reason,omitempty"`
}

var (
	reWhere     = regexp.MustCompile(`\bWHERE\b`)
	reDeleteCTE = regexp.MustCompile(`\bDELETE\s+FROM\b`)
	reUpdateCTE = regexp.MustCompile(`\bUPDATE\b[\s\S]*\bSET\b`)
	readKW      = map[string]bool{"SELECT": true, "SHOW": true, "DESC": true, "DESCRIBE": true, "EXPLAIN": true, "VALUES": true, "TABLE": true, "WITH": true}
	dangerKW    = map[string]string{"DROP": "DROP 删除对象", "TRUNCATE": "TRUNCATE 清空表", "ALTER": "ALTER 变更结构", "RENAME": "RENAME 重命名对象",
		"GRANT": "GRANT 授权", "REVOKE": "REVOKE 回收权限"}
)

// normalizeSQL strips comments, blanks string/identifier literal contents and
// returns the upper-cased statement plus the number of top-level statements.
func normalizeSQL(s string, mysqlDialect bool) (string, int) {
	var b strings.Builder
	n := len(s)
	stmts, nonEmpty := 0, false
	for i := 0; i < n; i++ {
		ch := s[i]
		switch {
		case ch == '-' && i+1 < n && s[i+1] == '-', ch == '#' && mysqlDialect:
			for i < n && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case ch == '/' && i+1 < n && s[i+1] == '*':
			i += 2
			for i+1 < n && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
		case ch == '\'' || ch == '"' || ch == '`':
			q := ch
			b.WriteByte(q)
			for i++; i < n; i++ {
				if s[i] == '\\' && mysqlDialect {
					i++
					continue
				}
				if s[i] == q {
					if i+1 < n && s[i+1] == q { // doubled quote escape
						i++
						continue
					}
					break
				}
			}
			b.WriteByte(q)
			nonEmpty = true
		case ch == ';':
			if nonEmpty {
				stmts++
			}
			nonEmpty = false
			b.WriteByte(' ')
		default:
			if !unicode.IsSpace(rune(ch)) {
				nonEmpty = true
			}
			b.WriteByte(ch)
		}
	}
	if nonEmpty {
		stmts++
	}
	return strings.ToUpper(strings.TrimSpace(b.String())), stmts
}

// AnalyzeSQL classifies a single statement; it errors on empty input or
// multiple statements (the console executes exactly one statement per call).
func AnalyzeSQL(stmt, dbType string) (SQLRisk, error) {
	norm, count := normalizeSQL(stmt, dbType == TypeMySQL)
	if count == 0 {
		return SQLRisk{}, errors.New("empty statement")
	}
	if count > 1 {
		return SQLRisk{}, errors.New("only one statement per execution is allowed")
	}
	kw := norm
	if i := strings.IndexFunc(norm, func(r rune) bool { return unicode.IsSpace(r) || r == '(' }); i > 0 {
		kw = norm[:i]
	}
	r := SQLRisk{Level: RiskWrite, Keyword: kw}
	switch {
	case dangerKW[kw] != "":
		r.Level, r.Reason = RiskDangerous, dangerKW[kw]
	case kw == "DELETE" || kw == "UPDATE":
		if !reWhere.MatchString(norm) {
			r.Level, r.Reason = RiskDangerous, kw+" 未带 WHERE 条件（全表）"
		}
	case kw == "WITH":
		if reDeleteCTE.MatchString(norm) || reUpdateCTE.MatchString(norm) {
			if !reWhere.MatchString(norm) {
				r.Level, r.Reason = RiskDangerous, "CTE 中 DELETE/UPDATE 未带 WHERE"
			}
		} else {
			r.Level = RiskRead
		}
	case readKW[kw]:
		r.Level = RiskRead
		if kw == "EXPLAIN" && strings.Contains(norm, "ANALYZE") && !strings.Contains(norm, "SELECT") {
			r.Level = RiskWrite // EXPLAIN ANALYZE executes the statement
		}
		if strings.Contains(norm, " INTO ") && kw == "SELECT" && (strings.Contains(norm, "OUTFILE") || strings.Contains(norm, "DUMPFILE")) {
			r.Level, r.Reason = RiskDangerous, "SELECT INTO OUTFILE 写文件"
		}
	}
	return r, nil
}

// ---------------------------------------------------------------- SQL execution

func sqlOpen(inst *Instance, database string) (*sql.DB, error) {
	addr := inst.Endpoints[0]
	if database == "" {
		database = inst.Options["database"]
	}
	switch inst.Type {
	case TypeMySQL:
		cfg := mysql.NewConfig()
		cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = inst.Auth.Username, inst.Auth.Password, "tcp", addr, database
		cfg.Timeout, cfg.ParseTime, cfg.MultiStatements = 5*time.Second, true, false
		if inst.Auth.TLS {
			cfg.TLSConfig = "true"
			if inst.Auth.TLSSkipVerify {
				cfg.TLSConfig = "skip-verify"
			}
		}
		return sql.Open("mysql", cfg.FormatDSN())
	case TypePostgres:
		if database == "" {
			database = "postgres"
		}
		sslmode := inst.Options["sslmode"]
		if sslmode == "" {
			sslmode = "disable"
			if inst.Auth.TLS {
				sslmode = "require"
			}
		}
		u := url.URL{Scheme: "postgres", User: url.UserPassword(inst.Auth.Username, inst.Auth.Password), Host: addr, Path: "/" + database,
			RawQuery: url.Values{"sslmode": {sslmode}, "connect_timeout": {"5"}, "application_name": {"xingshu-matrix-console"}}.Encode()}
		return sql.Open("pgx", u.String())
	}
	return nil, fmt.Errorf("instance type %s is not a database", inst.Type)
}

func sqlPing(ctx context.Context, inst *Instance) (string, error) {
	db, err := sqlOpen(inst, "")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var ver string
	q := "SELECT version()"
	if err := db.QueryRowContext(ctx, q).Scan(&ver); err != nil {
		return "", err
	}
	return ver, nil
}

// SQLResult is the console output.
type SQLResult struct {
	Risk         SQLRisk  `json:"risk"`
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowCount     int      `json:"row_count"`
	Truncated    bool     `json:"truncated"`
	RowsAffected int64    `json:"rows_affected"`
	DurationMS   int64    `json:"duration_ms"`
	AuditID      int64    `json:"audit_id,omitempty"`
}

// runSQL executes one statement on a dedicated connection with a server-side
// and client-side timeout; reads return at most maxRows rows.
func runSQL(ctx context.Context, inst *Instance, database, stmt string, risk SQLRisk, timeoutSec int) (*SQLResult, error) {
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeoutSec
	}
	if timeoutSec > maxTimeoutSec {
		timeoutSec = maxTimeoutSec
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	db, err := sqlOpen(inst, database)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ms := timeoutSec * 1000
	if inst.Type == TypeMySQL {
		_, _ = conn.ExecContext(ctx, "SET SESSION max_execution_time = "+strconv.Itoa(ms)) // MySQL 5.7.8+, SELECT only
	} else {
		_, _ = conn.ExecContext(ctx, "SET statement_timeout = "+strconv.Itoa(ms))
	}
	start := time.Now()
	res := &SQLResult{Risk: risk, Columns: []string{}, Rows: [][]any{}}
	if risk.Level != RiskRead {
		r, err := conn.ExecContext(ctx, stmt)
		res.DurationMS = time.Since(start).Milliseconds()
		if err != nil {
			return res, err
		}
		res.RowsAffected, _ = r.RowsAffected()
		return res, nil
	}
	rows, err := conn.QueryContext(ctx, stmt)
	if err != nil {
		res.DurationMS = time.Since(start).Milliseconds()
		return res, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	res.Columns = cols
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return res, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	res.RowCount = len(res.Rows)
	res.DurationMS = time.Since(start).Milliseconds()
	return res, rows.Err()
}

type sqlInput struct {
	SQL        string `json:"sql" binding:"required,max=200000"`
	Database   string `json:"database"`
	TimeoutSec int    `json:"timeout_sec"`
	Confirm    bool   `json:"confirm"` // required for dangerous statements on non-production envs
	Reason     string `json:"reason"`  // approval reason (production)
}

// sqlPayload is the approval payload of type middleware.sql.
type sqlPayload struct {
	InstanceID int64   `json:"instance_id"`
	Instance   string  `json:"instance"`
	Database   string  `json:"database"`
	SQL        string  `json:"sql"`
	TimeoutSec int     `json:"timeout_sec"`
	Risk       SQLRisk `json:"risk"`
}

func (m *Module) sqlConsole(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeMySQL, TypePostgres)
	if !ok {
		return
	}
	var in sqlInput
	if !httpx.Bind(c, &in) {
		return
	}
	ctx := c.Request.Context()
	p := rbac.PrincipalFrom(c)
	rec := auditRec{inst: inst, database: in.Database, stmt: in.SQL, p: p}
	risk, err := AnalyzeSQL(in.SQL, inst.Type)
	if err != nil {
		rec.status, rec.err = "blocked", err.Error()
		m.saveAudit(ctx, rec)
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	rec.risk = risk.Level
	audit.SetDetail(c, gin.H{"risk": risk, "database": in.Database})
	if risk.Level == RiskDangerous {
		needApproval := false
		if m.d.RBAC != nil {
			if needApproval, err = m.d.RBAC.RequiresApproval(ctx, inst.Env); err != nil {
				httpx.Fail(c, err)
				return
			}
		}
		switch {
		case needApproval:
			t, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{Type: approvalSQL,
				Title: fmt.Sprintf("[%s] 高危 SQL: %s", inst.Name, risk.Reason), Env: inst.Env, Reason: in.Reason,
				Payload: sqlPayload{InstanceID: inst.ID, Instance: inst.Name, Database: in.Database, SQL: in.SQL, TimeoutSec: in.TimeoutSec, Risk: risk}})
			if err != nil {
				httpx.Fail(c, err)
				return
			}
			if pending {
				rec.status, rec.approvalID = "pending", &t.ID
				m.saveAudit(ctx, rec)
				approval.RespondPending(c, t)
				return
			}
		case isProdEnv(inst.Env):
			rec.status, rec.err = "blocked", "production env has no approval policy: "+risk.Reason
			m.saveAudit(ctx, rec)
			httpx.Fail(c, httpx.Forbidden("生产环境禁止直接执行高危 SQL（"+risk.Reason+"），请为该环境开启审批策略后提交审批").WithDetails(risk))
			return
		case !in.Confirm:
			rec.status, rec.err = "blocked", "confirmation required: "+risk.Reason
			m.saveAudit(ctx, rec)
			httpx.Fail(c, httpx.Validation("高危 SQL（"+risk.Reason+"），请确认后以 confirm=true 重新提交").WithDetails(risk))
			return
		}
	}
	res, err := runSQL(ctx, inst, in.Database, in.SQL, risk, in.TimeoutSec)
	rec.fill(res, err)
	id := m.saveAudit(ctx, rec)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("SQL 执行失败: "+err.Error()).WithDetails(gin.H{"audit_id": id, "risk": risk}))
		return
	}
	res.AuditID = id
	httpx.OK(c, res)
}

func (m *Module) onSQLApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var pl sqlPayload
	if err := t.DecodePayload(&pl); err != nil {
		return nil, err
	}
	inst, err := m.loadInstance(ctx, pl.InstanceID)
	if err != nil {
		return nil, err
	}
	risk, err := AnalyzeSQL(pl.SQL, inst.Type)
	if err != nil {
		return nil, err
	}
	p := rbac.FromContext(ctx)
	if p == nil {
		p = &rbac.Principal{UserID: t.ApplicantID, Username: t.ApplicantName}
	}
	res, err := runSQL(ctx, inst, pl.Database, pl.SQL, risk, pl.TimeoutSec)
	rec := auditRec{inst: inst, database: pl.Database, stmt: pl.SQL, p: p, risk: risk.Level, approvalID: &t.ID}
	rec.fill(res, err)
	id := m.saveAudit(ctx, rec)
	if err != nil {
		return nil, err
	}
	res.AuditID = id
	return res, nil
}

// ---------------------------------------------------------------- Redis console

// redisBlocked maps blocked commands ("CMD" or "CMD SUB") to the reason.
var redisBlocked = map[string]string{
	"FLUSHALL": "清空所有库", "FLUSHDB": "清空当前库", "KEYS": "KEYS 会阻塞实例，请使用 SCAN",
	"CONFIG SET": "修改运行配置", "CONFIG REWRITE": "改写配置文件", "CONFIG RESETSTAT": "重置统计",
	"SHUTDOWN": "关闭实例", "DEBUG": "调试命令", "MONITOR": "长连接阻塞命令", "SUBSCRIBE": "长连接阻塞命令",
	"PSUBSCRIBE": "长连接阻塞命令", "SSUBSCRIBE": "长连接阻塞命令", "SYNC": "复制命令", "PSYNC": "复制命令",
	"REPLICAOF": "变更主从", "SLAVEOF": "变更主从", "SWAPDB": "交换库", "MODULE": "加载模块",
	"SCRIPT FLUSH": "清空脚本缓存", "FUNCTION FLUSH": "清空函数库", "CLUSTER RESET": "重置集群节点",
	"CLUSTER FAILOVER": "集群故障切换", "CLUSTER FORGET": "移除集群节点", "ACL SETUSER": "修改 ACL", "ACL DELUSER": "删除 ACL 用户",
	"CLIENT KILL": "断开客户端", "BGREWRITEAOF": "触发 AOF 重写", "BGSAVE": "触发 RDB 持久化", "SAVE": "阻塞式持久化",
}

var redisReadCmds = map[string]bool{"GET": true, "MGET": true, "EXISTS": true, "TTL": true, "PTTL": true, "TYPE": true, "STRLEN": true,
	"HGET": true, "HMGET": true, "HGETALL": true, "HKEYS": true, "HVALS": true, "HLEN": true, "HEXISTS": true, "HSCAN": true,
	"LRANGE": true, "LLEN": true, "LINDEX": true, "SMEMBERS": true, "SCARD": true, "SISMEMBER": true, "SSCAN": true,
	"ZRANGE": true, "ZREVRANGE": true, "ZRANGEBYSCORE": true, "ZCARD": true, "ZSCORE": true, "ZRANK": true, "ZSCAN": true, "ZCOUNT": true,
	"SCAN": true, "INFO": true, "DBSIZE": true, "PING": true, "TIME": true, "MEMORY": true, "OBJECT": true, "SLOWLOG": true,
	"XLEN": true, "XRANGE": true, "XREVRANGE": true, "XINFO": true, "GETRANGE": true, "BITCOUNT": true, "PFCOUNT": true,
	"CLIENT": true, "CONFIG": true, "CLUSTER": true, "COMMAND": true, "LATENCY": true, "ROLE": true, "RANDOMKEY": true}

// ParseRedisCommand splits a command line honouring single/double quotes.
func ParseRedisCommand(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inQ, has := byte(0), false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case inQ != 0:
			if ch == '\\' && i+1 < len(line) && inQ == '"' {
				i++
				switch line[i] {
				case 'n':
					cur.WriteByte('\n')
				case 't':
					cur.WriteByte('\t')
				default:
					cur.WriteByte(line[i])
				}
			} else if ch == inQ {
				inQ = 0
			} else {
				cur.WriteByte(ch)
			}
		case ch == '"' || ch == '\'':
			inQ, has = ch, true
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			if has {
				args = append(args, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(ch)
			has = true
		}
	}
	if inQ != 0 {
		return nil, errors.New("unbalanced quotes")
	}
	if has {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	return args, nil
}

// CheckRedisCommand returns the risk level, or an error if the command is blocked.
func CheckRedisCommand(args []string) (string, error) {
	cmd := strings.ToUpper(args[0])
	if r, ok := redisBlocked[cmd]; ok {
		return RiskDangerous, fmt.Errorf("命令 %s 已被禁止：%s", cmd, r)
	}
	if len(args) > 1 {
		sub := cmd + " " + strings.ToUpper(args[1])
		if r, ok := redisBlocked[sub]; ok {
			return RiskDangerous, fmt.Errorf("命令 %s 已被禁止：%s", sub, r)
		}
	}
	if redisReadCmds[cmd] {
		return RiskRead, nil
	}
	return RiskWrite, nil
}

// redisClient builds a client: one endpoint = standalone (db selectable),
// several = cluster, options.master_name = sentinel.
func redisClient(inst *Instance, db int) redis.UniversalClient {
	if db < 0 {
		db, _ = strconv.Atoi(inst.Options["db"])
	}
	o := &redis.UniversalOptions{Addrs: inst.Endpoints, Username: inst.Auth.Username, Password: inst.Auth.Password, DB: db,
		MasterName: inst.Options["master_name"], DialTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, Protocol: 2}
	if inst.Auth.TLS {
		o.TLSConfig = tlsConfig(inst)
	}
	return redis.NewUniversalClient(o)
}

func redisValue(v any, budget *int) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			if *budget <= 0 {
				out = append(out, "...(truncated)")
				break
			}
			*budget--
			out = append(out, redisValue(e, budget))
		}
		return out
	case map[any]any:
		out := map[string]any{}
		for k, e := range x {
			out[fmt.Sprint(k)] = redisValue(e, budget)
		}
		return out
	case []byte:
		return string(x)
	}
	return v
}

type redisInput struct {
	Command string   `json:"command"` // full command line, e.g. `HGET user:1 name`
	Args    []string `json:"args"`    // alternatively pre-split args
	DB      *int     `json:"db"`
}

func (m *Module) redisConsole(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeRedis)
	if !ok {
		return
	}
	var in redisInput
	if !httpx.Bind(c, &in) {
		return
	}
	ctx := c.Request.Context()
	args := in.Args
	var err error
	if len(args) == 0 {
		if args, err = ParseRedisCommand(in.Command); err != nil {
			httpx.Fail(c, httpx.Validation(err.Error()))
			return
		}
	}
	db := -1
	if in.DB != nil {
		db = *in.DB
	}
	rec := auditRec{inst: inst, database: strconv.Itoa(db), stmt: strings.Join(args, " "), p: rbac.PrincipalFrom(c)}
	if db < 0 {
		rec.database = inst.Options["db"]
	}
	risk, err := CheckRedisCommand(args)
	rec.risk = risk
	if err != nil {
		rec.status, rec.err = "blocked", err.Error()
		m.saveAudit(ctx, rec)
		httpx.Fail(c, httpx.Forbidden(err.Error()))
		return
	}
	cl := redisClient(inst, db)
	defer cl.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	iargs := make([]any, len(args))
	for i, a := range args {
		iargs[i] = a
	}
	start := time.Now()
	v, err := cl.Do(cctx, iargs...).Result()
	dur := time.Since(start).Milliseconds()
	if errors.Is(err, redis.Nil) {
		v, err = nil, nil
	}
	rec.durationMS = dur
	if err != nil {
		rec.status, rec.err = "error", err.Error()
		m.saveAudit(ctx, rec)
		httpx.Fail(c, httpx.BadRequest("Redis 执行失败: "+err.Error()))
		return
	}
	rec.status = "ok"
	id := m.saveAudit(ctx, rec)
	budget := maxRows
	httpx.OK(c, gin.H{"result": redisValue(v, &budget), "risk": risk, "duration_ms": dur, "audit_id": id})
}

// ---------------------------------------------------------------- console audit

type auditRec struct {
	inst       *Instance
	database   string
	stmt       string
	risk       string
	status     string
	err        string
	rows       int64
	durationMS int64
	approvalID *int64
	p          *rbac.Principal
}

func (r *auditRec) fill(res *SQLResult, err error) {
	r.status = "ok"
	if err != nil {
		r.status, r.err = "error", err.Error()
	}
	if res != nil {
		r.durationMS = res.DurationMS
		r.rows = res.RowsAffected
		if res.Risk.Level == RiskRead {
			r.rows = int64(res.RowCount)
		}
	}
}

// saveAudit persists a console record (never cancelled by the request ctx).
func (m *Module) saveAudit(ctx context.Context, r auditRec) int64 {
	if m.d == nil || m.d.DB == nil {
		return 0
	}
	var uid int64
	var uname string
	if r.p != nil {
		uid, uname = r.p.UserID, r.p.Username
	}
	var id int64
	err := m.d.DB.QueryRow(context.WithoutCancel(ctx), `INSERT INTO middleware_sql_audit(instance_id,instance_name,instance_type,env,database_name,statement,risk,status,error,rows_count,duration_ms,approval_id,user_id,username)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		r.inst.ID, r.inst.Name, r.inst.Type, r.inst.Env, r.database, r.stmt, r.risk, r.status, r.err, r.rows, r.durationMS, r.approvalID, uid, uname).Scan(&id)
	if err != nil && m.d.Logger != nil {
		m.d.Logger.Error("save console audit", "err", err)
	}
	return id
}

// AuditEntry is a row of middleware_sql_audit.
type AuditEntry struct {
	ID           int64     `json:"id"`
	InstanceID   int64     `json:"instance_id"`
	InstanceName string    `json:"instance_name"`
	InstanceType string    `json:"instance_type"`
	Env          string    `json:"env"`
	Database     string    `json:"database"`
	Statement    string    `json:"statement"`
	Risk         string    `json:"risk"`
	Status       string    `json:"status"`
	Error        string    `json:"error"`
	Rows         int64     `json:"rows"`
	DurationMS   int64     `json:"duration_ms"`
	ApprovalID   *int64    `json:"approval_id"`
	UserID       int64     `json:"user_id"`
	Username     string    `json:"username"`
	CreatedAt    time.Time `json:"created_at"`
}

func (m *Module) listAudit(c *gin.Context) {
	p := httpx.ParsePage(c)
	where, args := []string{"1=1"}, []any{}
	add := func(col, v string) {
		if v != "" {
			args = append(args, v)
			where = append(where, fmt.Sprintf("%s=$%d", col, len(args)))
		}
	}
	add("instance_id::text", c.Query("instance_id"))
	add("username", c.Query("username"))
	add("status", c.Query("status"))
	add("risk", c.Query("risk"))
	add("instance_type", c.Query("type"))
	if pr := rbac.PrincipalFrom(c); pr.EnvScope != nil && !contains(pr.EnvScope, "*") {
		args = append(args, pr.EnvScope)
		where = append(where, fmt.Sprintf("env = ANY($%d)", len(args)))
	}
	w := strings.Join(where, " AND ")
	ctx := c.Request.Context()
	var total int64
	if err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM middleware_sql_audit WHERE `+w, args...).Scan(&total); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	args = append(args, p.Limit(), p.Offset())
	rows, err := m.d.DB.Query(ctx, fmt.Sprintf(`SELECT id,instance_id,instance_name,instance_type,env,database_name,statement,risk,status,error,rows_count,duration_ms,approval_id,user_id,username,created_at
		FROM middleware_sql_audit WHERE %s ORDER BY id DESC LIMIT $%d OFFSET $%d`, w, len(args)-1, len(args)), args...)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	defer rows.Close()
	var items []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.InstanceID, &e.InstanceName, &e.InstanceType, &e.Env, &e.Database, &e.Statement, &e.Risk, &e.Status,
			&e.Error, &e.Rows, &e.DurationMS, &e.ApprovalID, &e.UserID, &e.Username, &e.CreatedAt); err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		items = append(items, e)
	}
	httpx.OK(c, httpx.NewPageResult(items, total, p))
}
