package bastion

// 数据库控制台（对应 JumpServer 工作台「数据库连接」+ 工单「SQL 审核」）。
//
// 对 MySQL / PostgreSQL 资产按 ClassifySQL 分级路由：
//   read      → 在数据库只读事务中执行，限制返回行数与超时，结果按脱敏规则替换后返回；
//   write/high→ 生成审批单（类型 bastion.sql），审批通过后由处理器在普通事务中执行并记录；
//   forbidden → 直接拒绝。
// 每条 SQL（无论结果）都写入 bastion_db_queries；SQL 原文加密保存，接口只返回去掉字面量的摘要。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql 驱动 "pgx"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// SQLApprovalType 是数据库变更审批单类型。
const SQLApprovalType = "bastion.sql"

const (
	dbDefaultLimit   = 200
	dbMaxLimit       = 1000
	dbDefaultTimeout = 15 * time.Second
	dbMaxTimeout     = 60 * time.Second
	dbWriteTimeout   = 5 * time.Minute
)

// DBResult 是一次 SQL 执行的结果。
type DBResult struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowCount     int      `json:"row_count"`
	Truncated    bool     `json:"truncated"`
	RowsAffected int64    `json:"rows_affected"`
	DurationMS   int64    `json:"duration_ms"`
}

// sqlRunner 执行 SQL。ReadOnly 必须在数据库只读事务中运行，Exec 在普通事务中运行并提交。
type sqlRunner interface {
	ReadOnly(ctx context.Context, stmt string, limit int) (*DBResult, error)
	Exec(ctx context.Context, stmt string) (*DBResult, error)
}

// dbOutcome 是执行器的路由结果。
type dbOutcome struct {
	Level    string    `json:"level"`
	Reason   string    `json:"reason"`
	Status   string    `json:"status"` // executed | failed | rejected | pending
	Result   *DBResult `json:"result,omitempty"`
	TicketID int64     `json:"ticket_id,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// dbExecutor 按 SQL 分级把语句路由到只读执行、审批或拒绝。
type dbExecutor struct {
	runner sqlRunner
	// submit 为变更语句创建审批单并返回单号；approved=true 时不会调用。
	submit func(ctx context.Context, level, reason string) (int64, error)
	mask   *Masker
	limit  int
}

// Run 执行一条语句。approved 表示这是审批通过后的执行（只对 write/high 有意义）。
func (e *dbExecutor) Run(ctx context.Context, stmt string, approved bool) dbOutcome {
	level, reason := ClassifySQL(stmt)
	out := dbOutcome{Level: level, Reason: reason}
	switch level {
	case SQLForbidden:
		out.Status, out.Error = "rejected", "禁止执行："+reason
		return out
	case SQLRead:
		limit := e.limit
		if limit <= 0 || limit > dbMaxLimit {
			limit = dbDefaultLimit
		}
		res, err := e.runner.ReadOnly(ctx, stmt, limit)
		out.Result = maskResult(res, e.mask)
		if err != nil {
			out.Status, out.Error = "failed", dbErrReasonFull(err)
			return out
		}
		out.Status = "executed"
		return out
	}
	// write / high
	if !approved {
		if e.submit == nil {
			out.Status, out.Error = "rejected", "审批服务不可用，变更已拒绝"
			return out
		}
		id, err := e.submit(ctx, level, reason)
		if err != nil {
			out.Status, out.Error = "rejected", "提交审批失败："+err.Error()
			return out
		}
		out.Status, out.TicketID = "pending", id
		return out
	}
	res, err := e.runner.Exec(ctx, stmt)
	out.Result = res
	if err != nil {
		out.Status, out.Error = "failed", dbErrReasonFull(err)
		return out
	}
	out.Status = "executed"
	return out
}

// dbErrReasonFull 保留数据库返回的错误文本（只读查询的错误对使用者有用），但截断长度。
func dbErrReasonFull(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "执行超时，已取消"
	}
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// maskResult 对结果中的文本单元格应用脱敏规则。
func maskResult(r *DBResult, mk *Masker) *DBResult {
	if r == nil || mk == nil || len(mk.rules) == 0 {
		return r
	}
	for _, row := range r.Rows {
		for i, v := range row {
			if s, ok := v.(string); ok {
				row[i] = string(mk.Apply([]byte(s)))
			}
		}
	}
	return r
}

// ---------- 真实执行 ----------

type sqlDBRunner struct {
	db      *sql.DB
	kind    string // mysql | postgres
	timeout time.Duration
}

func (r *sqlDBRunner) conn(ctx context.Context) (*sql.Conn, error) {
	r.db.SetMaxOpenConns(1)
	return r.db.Conn(ctx)
}

func (r *sqlDBRunner) ReadOnly(ctx context.Context, stmt string, limit int) (*DBResult, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	conn, err := r.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ms := strconv.FormatInt(r.timeout.Milliseconds(), 10)
	if r.kind == "mysql" {
		_, _ = conn.ExecContext(ctx, "SET SESSION max_execution_time = "+ms)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 只读事务总是回滚
	if r.kind == "postgres" {
		if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = "+ms); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	res := &DBResult{Columns: []string{}, Rows: [][]any{}}
	rows, err := tx.QueryContext(ctx, stmt)
	if err != nil {
		res.DurationMS = time.Since(start).Milliseconds()
		return res, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	res.Columns = cols
	for rows.Next() {
		if len(res.Rows) >= limit {
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
			vals[i] = cellValue(v)
		}
		res.Rows = append(res.Rows, vals)
	}
	res.RowCount = len(res.Rows)
	res.DurationMS = time.Since(start).Milliseconds()
	if !res.Truncated {
		return res, rows.Err()
	}
	return res, nil
}

func (r *sqlDBRunner) Exec(ctx context.Context, stmt string) (*DBResult, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	conn, err := r.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚是空操作
	if r.kind == "postgres" {
		if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(r.timeout.Milliseconds(), 10)); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	res := &DBResult{Columns: []string{}, Rows: [][]any{}}
	er, err := tx.ExecContext(ctx, stmt)
	res.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		return res, err
	}
	res.RowsAffected, _ = er.RowsAffected()
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

func cellValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999 -07:00")
	}
	return v
}

// openDB 用托管账号打开到数据库资产的连接（不做连接池复用，用完即关）。
func (m *Module) openDB(t *rotationTarget, database string) (*sql.DB, error) {
	pw, err := m.d.Crypto.Decrypt(t.acc.Secret)
	if err != nil {
		return nil, errors.New("credential unavailable")
	}
	opt := func(k string) string {
		if t.asset.Options == nil {
			return ""
		}
		if v, ok := t.asset.Options[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	if database == "" {
		database = opt("database")
	}
	addr := net.JoinHostPort(t.asset.Host, strconv.Itoa(t.asset.Port))
	switch t.asset.Protocol {
	case "mysql":
		cfg := mysql.NewConfig()
		cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = t.acc.Username, pw, "tcp", addr, database
		cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, dbWriteTimeout, dbWriteTimeout
		cfg.ParseTime, cfg.MultiStatements = true, false
		if tls := opt("tls"); tls == "true" || tls == "skip-verify" || tls == "preferred" {
			cfg.TLSConfig = tls
		}
		return sql.Open("mysql", cfg.FormatDSN())
	case "postgres":
		if database == "" {
			database = "postgres"
		}
		ssl := opt("sslmode")
		if ssl == "" {
			ssl = "prefer"
		}
		u := url.URL{Scheme: "postgres", User: url.UserPassword(t.acc.Username, pw), Host: addr, Path: "/" + database,
			RawQuery: url.Values{"sslmode": {ssl}, "connect_timeout": {"5"}, "application_name": {"xingshu-bastion"}}.Encode()}
		return sql.Open("pgx", u.String())
	}
	return nil, errors.New("asset is not a database")
}

// ---------- HTTP ----------

type dbTarget struct {
	*rotationTarget
	database string
}

// dbAccess 校验数据库资产、环境、授权与登录 ACL；失败时已写响应并返回 nil。
func (m *Module) dbAccess(c *gin.Context, assetID, accountID int64, database string) *dbTarget {
	p := rbac.PrincipalFrom(c)
	t, err := m.loadTarget(c, accountID)
	if err != nil {
		httpx.Fail(c, err)
		return nil
	}
	if t.asset.ID != assetID {
		httpx.Fail(c, httpx.Validation("account does not belong to asset"))
		return nil
	}
	if t.asset.Protocol != "mysql" && t.asset.Protocol != "postgres" {
		httpx.Fail(c, httpx.Validation("asset is not a MySQL / PostgreSQL database"))
		return nil
	}
	if !p.CanEnv(t.asset.Env) {
		httpx.Fail(c, httpx.Forbidden("environment "+t.asset.Env+" not in your scope"))
		return nil
	}
	logIn := loginLog{UserID: p.UserID, Username: p.Username, AssetID: t.asset.ID, AssetName: t.asset.Name, Account: t.acc.Username, Protocol: t.asset.Protocol, ClientIP: c.ClientIP()}
	if okk, err := m.authorized(c, p, assetID, accountID); err != nil {
		httpx.Fail(c, err)
		return nil
	} else if !okk {
		logIn.Result, logIn.Reason = "denied", "没有生效的资产授权"
		m.writeLoginLog(logIn)
		httpx.Fail(c, httpx.Forbidden("no active authorization rule for this asset/account"))
		return nil
	}
	if reason, err := m.checkACL(c, p, t.asset.ID, c.ClientIP()); err != nil {
		httpx.Fail(c, err)
		return nil
	} else if reason != "" {
		logIn.Result, logIn.Reason = "denied", reason
		m.writeLoginLog(logIn)
		httpx.Fail(c, httpx.Forbidden(reason))
		return nil
	}
	if len(database) > 128 || strings.ContainsAny(database, "/?#;'\"`\\ ") {
		httpx.Fail(c, httpx.Validation("invalid database name"))
		return nil
	}
	return &dbTarget{rotationTarget: t, database: database}
}

type dbConnectIn struct {
	AssetID   int64  `json:"asset_id" binding:"required,min=1"`
	AccountID int64  `json:"account_id" binding:"required,min=1"`
	Database  string `json:"database"`
}

// dbConnect 打开数据库控制台：验证授权与 ACL、真实连接一次并记录登录日志。
func (m *Module) dbConnect(c *gin.Context) {
	var in dbConnectIn
	if !httpx.Bind(c, &in) {
		return
	}
	t := m.dbAccess(c, in.AssetID, in.AccountID, in.Database)
	if t == nil {
		return
	}
	p := rbac.PrincipalFrom(c)
	logIn := loginLog{UserID: p.UserID, Username: p.Username, AssetID: t.asset.ID, AssetName: t.asset.Name, Account: t.acc.Username, Protocol: t.asset.Protocol, ClientIP: c.ClientIP()}
	ctx, cancel := context.WithTimeout(c, 10*time.Second)
	defer cancel()
	db, err := m.openDB(t.rotationTarget, t.database)
	if err != nil {
		logIn.Result, logIn.Reason = "failed", "无法建立数据库连接"
		m.writeLoginLog(logIn)
		httpx.Fail(c, httpx.Conflict("无法建立数据库连接"))
		return
	}
	defer db.Close()
	var version string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&version); err != nil {
		logIn.Result, logIn.Reason = "failed", dbErrReason(err)
		m.writeLoginLog(logIn)
		httpx.Fail(c, httpx.Conflict(dbErrReason(err)))
		return
	}
	var dbs []string
	listSQL := "SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1"
	if t.asset.Protocol == "mysql" {
		listSQL = "SHOW DATABASES"
	}
	if rows, err := db.QueryContext(ctx, listSQL); err == nil {
		for rows.Next() && len(dbs) < 500 {
			var n string
			if rows.Scan(&n) == nil {
				dbs = append(dbs, n)
			}
		}
		rows.Close()
	}
	logIn.Result = "success"
	m.writeLoginLog(logIn)
	audit.SetResource(c, "bastion_asset", itoa(t.asset.ID))
	def := t.database
	if def == "" && t.asset.Options != nil {
		if v, ok := t.asset.Options["database"]; ok && v != nil {
			def = fmt.Sprint(v)
		}
	}
	ok(c, gin.H{"version": version, "databases": dbs, "default_database": def, "protocol": t.asset.Protocol,
		"limits": gin.H{"max_rows": dbMaxLimit, "default_rows": dbDefaultLimit, "max_timeout_sec": int(dbMaxTimeout.Seconds())}})
}

type dbQueryIn struct {
	AssetID    int64  `json:"asset_id" binding:"required,min=1"`
	AccountID  int64  `json:"account_id" binding:"required,min=1"`
	Database   string `json:"database"`
	SQL        string `json:"sql" binding:"required,max=100000"`
	Reason     string `json:"reason" binding:"max=2000"`
	Limit      int    `json:"limit"`
	TimeoutSec int    `json:"timeout_sec"`
}

type sqlApprovalPayload struct {
	QueryID      int64  `json:"query_id"`
	AssetID      int64  `json:"asset_id"`
	Asset        string `json:"asset"`
	AccountID    int64  `json:"account_id"`
	Account      string `json:"account"`
	Database     string `json:"database"`
	Level        string `json:"level"`
	Risk         string `json:"risk"`
	Summary      string `json:"summary"`
	EncryptedSQL string `json:"encrypted_sql,omitempty"`
}

func sqlAAD(queryID int64) []byte { return []byte("bastion:sql:" + strconv.FormatInt(queryID, 10)) }

// sqlSummary 返回去掉注释与字面量后的 SQL 摘要（不含数据值），用于列表与审批单展示。
func sqlSummary(q string) string {
	n := NormalizeSQL(q)
	if r := []rune(n); len(r) > 300 {
		n = string(r[:300]) + "…"
	}
	return n
}

func timeoutFrom(sec int) time.Duration {
	d := time.Duration(sec) * time.Second
	if d <= 0 {
		return dbDefaultTimeout
	}
	if d > dbMaxTimeout {
		return dbMaxTimeout
	}
	return d
}

func (m *Module) dbQuery(c *gin.Context) {
	var in dbQueryIn
	if !httpx.Bind(c, &in) {
		return
	}
	t := m.dbAccess(c, in.AssetID, in.AccountID, in.Database)
	if t == nil {
		return
	}
	p := rbac.PrincipalFrom(c)
	masker, err := m.loadMasker(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	level, _ := ClassifySQL(in.SQL)
	// 先登记记录（拿到 ID 作为加密附加数据），再路由执行
	var qid int64
	if err := m.d.DB.QueryRow(c, `INSERT INTO bastion_db_queries (user_id,username,asset_id,asset_name,account_id,account_user,database_name,level,reason,summary,status,client_ip)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending',$11) RETURNING id`, p.UserID, p.Username, t.asset.ID, t.asset.Name, t.acc.ID, t.acc.Username,
		t.database, level, strings.TrimSpace(in.Reason), sqlSummary(in.SQL), c.ClientIP()).Scan(&qid); err != nil {
		httpx.Fail(c, err)
		return
	}
	// 密文绑定记录 ID，防止被挪到别的记录上解密执行
	enc, err := m.d.Crypto.EncryptBytes([]byte(in.SQL), sqlAAD(qid))
	if err != nil {
		m.finishQuery(context.WithoutCancel(c.Request.Context()), qid, dbOutcome{Status: "failed", Error: "加密服务不可用"})
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	_, _ = m.d.DB.Exec(c, `UPDATE bastion_db_queries SET sql_enc=$2 WHERE id=$1`, qid, enc)

	var db *sql.DB
	runner := &lazyRunner{open: func() (sqlRunner, error) {
		var err error
		if db, err = m.openDB(t.rotationTarget, t.database); err != nil {
			return nil, err
		}
		return &sqlDBRunner{db: db, kind: t.asset.Protocol, timeout: timeoutFrom(in.TimeoutSec)}, nil
	}}
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	ex := &dbExecutor{runner: runner, mask: masker, limit: in.Limit}
	if m.d.Approval != nil {
		ex.submit = func(ctx context.Context, level, reason string) (int64, error) {
			if strings.TrimSpace(in.Reason) == "" {
				return 0, httpx.Validation("变更语句需要填写申请原因")
			}
			tk, err := m.d.Approval.CreateOwned(ctx, p, approval.CreateInput{
				Type:   SQLApprovalType,
				Title:  fmt.Sprintf("SQL 变更：%s/%s（%s）", t.asset.Name, firstNonEmpty(t.database, "默认库"), levelZH(level)),
				Env:    t.asset.Env,
				Reason: strings.TrimSpace(in.Reason),
				Payload: sqlApprovalPayload{QueryID: qid, AssetID: t.asset.ID, Asset: t.asset.Name, AccountID: t.acc.ID, Account: t.acc.Username,
					Database: t.database, Level: level, Risk: reason, Summary: sqlSummary(in.SQL), EncryptedSQL: enc},
			})
			if err != nil {
				return 0, err
			}
			return tk.ID, nil
		}
	}
	out := ex.Run(c.Request.Context(), in.SQL, false)
	m.finishQuery(context.WithoutCancel(c.Request.Context()), qid, out)
	audit.SetResource(c, "bastion_db_query", itoa(qid))
	audit.SetDetail(c, gin.H{"level": out.Level, "status": out.Status, "asset": t.asset.Name, "ticket_id": out.TicketID})
	body := gin.H{"query_id": qid, "level": out.Level, "risk": out.Reason, "status": out.Status, "result": out.Result, "ticket_id": out.TicketID, "error": out.Error}
	switch out.Status {
	case "pending":
		c.JSON(202, httpx.Envelope{Code: httpx.CodeApprovalRequired, Message: "变更语句已提交审批", Data: body, RequestID: httpx.RequestIDFrom(c)})
	case "rejected":
		httpx.Fail(c, httpx.Validation(out.Error).WithDetails(body))
	case "failed":
		httpx.Fail(c, httpx.BadRequest("SQL 执行失败："+out.Error).WithDetails(body))
	default:
		ok(c, body)
	}
}

// lazyRunner 只有真正需要执行时才连接数据库（禁止 / 审批路由不连库）。
type lazyRunner struct {
	open func() (sqlRunner, error)
	r    sqlRunner
}

func (l *lazyRunner) get() (sqlRunner, error) {
	if l.r == nil {
		r, err := l.open()
		if err != nil {
			return nil, err
		}
		l.r = r
	}
	return l.r, nil
}

func (l *lazyRunner) ReadOnly(ctx context.Context, stmt string, limit int) (*DBResult, error) {
	r, err := l.get()
	if err != nil {
		return nil, err
	}
	return r.ReadOnly(ctx, stmt, limit)
}

func (l *lazyRunner) Exec(ctx context.Context, stmt string) (*DBResult, error) {
	r, err := l.get()
	if err != nil {
		return nil, err
	}
	return r.Exec(ctx, stmt)
}

func (m *Module) finishQuery(ctx context.Context, id int64, out dbOutcome) {
	var rowsN int
	var affected, dur int64
	if out.Result != nil {
		rowsN, affected, dur = out.Result.RowCount, out.Result.RowsAffected, out.Result.DurationMS
	}
	var ticket any
	if out.TicketID > 0 {
		ticket = out.TicketID
	}
	finished := "now()"
	if out.Status == "pending" {
		finished = "NULL"
	}
	_, err := m.d.DB.Exec(ctx, `UPDATE bastion_db_queries SET status=$2,error=$3,row_count=$4,rows_affected=$5,duration_ms=$6,ticket_id=COALESCE($7,ticket_id),finished_at=`+finished+` WHERE id=$1`,
		id, out.Status, out.Error, rowsN, affected, dur, ticket)
	if err != nil && m.d.Logger != nil {
		m.d.Logger.Warn("bastion: db query audit not updated", "id", id, "error", err)
	}
}

func levelZH(l string) string {
	switch l {
	case SQLRead:
		return "只读"
	case SQLWrite:
		return "变更"
	case SQLHigh:
		return "高危"
	}
	return "禁止"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------- 审批处理 ----------

type sqlApprovalHandler struct{ m *Module }

// OnApproved 审批通过后执行变更：重新校验申请人的授权与环境、重新分级，然后在事务中执行。
func (h sqlApprovalHandler) OnApproved(ctx context.Context, tk *approval.Ticket) (any, error) {
	m := h.m
	var pl sqlApprovalPayload
	if err := tk.DecodePayload(&pl); err != nil || pl.QueryID < 1 {
		return nil, httpx.Validation("invalid SQL approval payload")
	}
	var status string
	var enc string
	if err := m.d.DB.QueryRow(ctx, `SELECT status, sql_enc FROM bastion_db_queries WHERE id=$1`, pl.QueryID).Scan(&status, &enc); err != nil {
		return nil, httpx.NotFound("SQL record not found")
	}
	if status != "pending" {
		return nil, httpx.Conflict("SQL record is not pending")
	}
	fail := func(msg string) (any, error) {
		m.finishQuery(ctx, pl.QueryID, dbOutcome{Status: "failed", Error: msg})
		return nil, errors.New(msg)
	}
	if m.d.RBAC == nil {
		return fail("权限服务不可用")
	}
	rolesList, perms, envScope, err := m.d.RBAC.UserAccess(ctx, tk.ApplicantID)
	if err != nil {
		return fail("无法读取申请人权限")
	}
	applicant := &rbac.Principal{UserID: tk.ApplicantID, Username: tk.ApplicantName, Roles: rolesList, Permissions: perms, EnvScope: envScope}
	t, err := m.loadTarget(ctx, pl.AccountID)
	if err != nil || t.asset.ID != pl.AssetID || t.asset.Env != tk.Env {
		return fail("资产或账号已变更，请重新提交")
	}
	if !applicant.Can(PermConnect) || !applicant.CanEnv(t.asset.Env) {
		return fail("申请人已没有连接权限或环境范围")
	}
	if okk, err := m.authorized(ctx, applicant, t.asset.ID, t.acc.ID); err != nil || !okk {
		return fail("申请人的资产授权已失效")
	}
	plain, err := m.d.Crypto.DecryptBytes(enc, sqlAAD(pl.QueryID))
	if err != nil {
		return fail("SQL 数据校验失败")
	}
	db, err := m.openDB(t, pl.Database)
	if err != nil {
		return fail("无法建立数据库连接")
	}
	defer db.Close()
	ex := &dbExecutor{runner: &sqlDBRunner{db: db, kind: t.asset.Protocol, timeout: dbWriteTimeout}}
	out := ex.Run(ctx, string(plain), true)
	if out.Status == "rejected" { // 分级规则在审批期间变严格
		out.Status = "failed"
	}
	m.finishQuery(ctx, pl.QueryID, out)
	if out.Status != "executed" {
		return nil, errors.New(firstNonEmpty(out.Error, "SQL 执行失败"))
	}
	return gin.H{"query_id": pl.QueryID, "rows_affected": out.Result.RowsAffected, "duration_ms": out.Result.DurationMS}, nil
}

// OnRejected 审批被拒绝或撤回时把记录标记为 rejected。
func (h sqlApprovalHandler) OnRejected(ctx context.Context, tk *approval.Ticket) error {
	var pl sqlApprovalPayload
	if err := tk.DecodePayload(&pl); err != nil || pl.QueryID < 1 {
		return nil
	}
	msg := "审批未通过"
	if tk.Status == approval.StatusCancelled {
		msg = "申请人已撤回"
	} else if tk.Comment != "" {
		msg += "：" + tk.Comment
	}
	_, err := h.m.d.DB.Exec(ctx, `UPDATE bastion_db_queries SET status='rejected', error=$2, finished_at=now() WHERE id=$1 AND status='pending'`, pl.QueryID, msg)
	return err
}

// sanitizeSQLPayload 从审批单接口里去掉加密 SQL。
func sanitizeSQLPayload(raw json.RawMessage) json.RawMessage {
	var pl sqlApprovalPayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return json.RawMessage(`{}`)
	}
	pl.EncryptedSQL = ""
	b, err := json.Marshal(pl)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// ---------- 查询记录 ----------

// DBQuery 是数据库操作记录（不含 SQL 原文）。
type DBQuery struct {
	ID           int64      `db:"id" json:"id"`
	UserID       int64      `db:"user_id" json:"user_id"`
	Username     string     `db:"username" json:"username"`
	AssetID      int64      `db:"asset_id" json:"asset_id"`
	AssetName    string     `db:"asset_name" json:"asset_name"`
	AccountID    int64      `db:"account_id" json:"account_id"`
	AccountUser  string     `db:"account_user" json:"account_user"`
	Database     string     `db:"database_name" json:"database"`
	Level        string     `db:"level" json:"level"`
	Reason       string     `db:"reason" json:"reason"`
	Summary      string     `db:"summary" json:"summary"`
	SQLEnc       string     `db:"sql_enc" json:"-"`
	Status       string     `db:"status" json:"status"`
	Error        string     `db:"error" json:"error"`
	RowCount     int        `db:"row_count" json:"row_count"`
	RowsAffected int64      `db:"rows_affected" json:"rows_affected"`
	DurationMS   int64      `db:"duration_ms" json:"duration_ms"`
	TicketID     *int64     `db:"ticket_id" json:"ticket_id"`
	ClientIP     string     `db:"client_ip" json:"client_ip"`
	CreatedAt    time.Time  `db:"created_at" json:"created_at"`
	FinishedAt   *time.Time `db:"finished_at" json:"finished_at"`
}

func (m *Module) listDBQueries(c *gin.Context, mine bool) {
	p := httpx.ParsePage(c)
	user := int64(0)
	if mine {
		user = rbac.PrincipalFrom(c).UserID
	}
	q := "%" + escapeLike(strings.TrimSpace(c.Query("q"))) + "%"
	where := `WHERE ($1=0 OR user_id=$1) AND ($2='' OR level=$2) AND ($3='' OR status=$3) AND ($4='' OR asset_id::text=$4)
		AND (summary ILIKE $5 OR username ILIKE $5 OR asset_name ILIKE $5)`
	args := []any{user, c.Query("level"), c.Query("status"), c.Query("asset_id"), q}
	items, err := queryAll[DBQuery](c, m, `SELECT * FROM bastion_db_queries `+where+` ORDER BY id DESC LIMIT $6 OFFSET $7`, append(args, p.Limit(), p.Offset())...)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var total int64
	_ = m.d.DB.QueryRow(c, `SELECT count(*) FROM bastion_db_queries `+where, args...).Scan(&total)
	ok(c, httpx.NewPageResult(items, total, p))
}
