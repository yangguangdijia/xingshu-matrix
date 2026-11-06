package bastion

// 资产管理补全（对应 JumpServer「资产管理」）：资产树节点、平台、账号连通性验证，以及控制台总览与全局命令记录。

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ---------- 资产树节点 ----------

// Node 是资产树上的一个节点。
type Node struct {
	ID         int64     `db:"id" json:"id"`
	ParentID   *int64    `db:"parent_id" json:"parent_id"`
	Name       string    `db:"name" json:"name"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	AssetCount int64     `db:"asset_count" json:"asset_count"` // 直接挂在该节点下的资产数
}

type nodeIn struct {
	Name     string `json:"name" binding:"required,max=64"`
	ParentID *int64 `json:"parent_id"`
}

// nodeNameOK 校验节点名称：1-64 个字符，不含 "/"。
func nodeNameOK(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && len([]rune(name)) <= 64 && !strings.Contains(name, "/")
}

func (m *Module) listNodes(c *gin.Context) {
	items, err := queryAll[Node](c, m, `SELECT n.id,n.parent_id,n.name,n.created_at,
		(SELECT count(*) FROM bastion_assets a WHERE a.node_id=n.id) AS asset_count
		FROM bastion_nodes n ORDER BY n.name, n.id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, items)
}

// nodeCycle 判断把 id 的父节点设为 parent 是否会形成环。
func (m *Module) nodeCycle(ctx context.Context, id, parent int64) (bool, error) {
	var n int
	err := m.d.DB.QueryRow(ctx, `WITH RECURSIVE up(id, parent_id) AS (
		SELECT id, parent_id FROM bastion_nodes WHERE id=$2
		UNION SELECT n.id, n.parent_id FROM bastion_nodes n JOIN up ON n.id=up.parent_id)
		SELECT count(*) FROM up WHERE id=$1`, id, parent).Scan(&n)
	return n > 0, err
}

func (m *Module) saveNode(c *gin.Context) {
	var in nodeIn
	if !httpx.Bind(c, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !nodeNameOK(in.Name) {
		httpx.Fail(c, httpx.Validation("node name must be 1-64 characters without '/'"))
		return
	}
	if in.ParentID != nil && *in.ParentID <= 0 {
		in.ParentID = nil
	}
	var n *Node
	var err error
	if c.Param("id") == "" {
		n, err = queryOne[Node](c, m, `INSERT INTO bastion_nodes (name,parent_id) VALUES ($1,$2) RETURNING id,parent_id,name,created_at,0::bigint AS asset_count`, in.Name, in.ParentID)
	} else {
		id, okk := httpx.ParamID(c, "id")
		if !okk {
			return
		}
		if in.ParentID != nil {
			if *in.ParentID == id {
				httpx.Fail(c, httpx.Validation("a node cannot be its own parent"))
				return
			}
			cyc, cerr := m.nodeCycle(c, id, *in.ParentID)
			if cerr != nil {
				httpx.Fail(c, cerr)
				return
			}
			if cyc {
				httpx.Fail(c, httpx.Validation("cannot move a node under its own descendant"))
				return
			}
		}
		n, err = queryOne[Node](c, m, `UPDATE bastion_nodes SET name=$2,parent_id=$3 WHERE id=$1
			RETURNING id,parent_id,name,created_at,(SELECT count(*) FROM bastion_assets a WHERE a.node_id=bastion_nodes.id) AS asset_count`, id, in.Name, in.ParentID)
	}
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_node", itoa(n.ID))
	if c.Param("id") == "" {
		httpx.Created(c, n)
		return
	}
	ok(c, n)
}

func (m *Module) deleteNode(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var kids, assets int
	if err := m.d.DB.QueryRow(c, `SELECT (SELECT count(*) FROM bastion_nodes WHERE parent_id=$1),(SELECT count(*) FROM bastion_assets WHERE node_id=$1)`, id).Scan(&kids, &assets); err != nil {
		httpx.Fail(c, err)
		return
	}
	if kids > 0 || assets > 0 {
		httpx.Fail(c, httpx.Conflict("节点下还有子节点或资产，请先移走后再删除").WithDetails(gin.H{"children": kids, "assets": assets}))
		return
	}
	if m.exec(c, `DELETE FROM bastion_nodes WHERE id=$1`, id) {
		audit.SetResource(c, "bastion_node", itoa(id))
		ok(c, gin.H{"id": id})
	}
}

// ---------- 平台 ----------

// Platform 描述资产的系统或数据库类型。
type Platform struct {
	ID          int64     `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	Category    string    `db:"category" json:"category"`
	Type        string    `db:"type" json:"type"`
	Protocol    string    `db:"protocol" json:"protocol"`
	Port        int       `db:"port" json:"port"`
	Charset     string    `db:"charset" json:"charset"`
	Description string    `db:"description" json:"description"`
	Builtin     bool      `db:"builtin" json:"builtin"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	AssetCount  int64     `db:"asset_count" json:"asset_count"`
	Connectable bool      `db:"-" json:"connectable"` // 平台的协议是否有可用连接器
}

type platformIn struct {
	Name        string `json:"name" binding:"required,max=64"`
	Type        string `json:"type" binding:"required,oneof=linux unix windows mysql postgresql"`
	Port        int    `json:"port" binding:"required,min=1,max=65535"`
	Charset     string `json:"charset" binding:"max=32"`
	Description string `json:"description" binding:"max=500"`
}

// platformKind 由类型推出分类与协议。
func platformKind(t string) (category, protocol string) {
	switch t {
	case "mysql":
		return "database", "mysql"
	case "postgresql":
		return "database", "postgres"
	case "windows":
		return "host", "rdp"
	}
	return "host", "ssh"
}

func (m *Module) listPlatforms(c *gin.Context) {
	items, err := queryAll[Platform](c, m, `SELECT p.*,(SELECT count(*) FROM bastion_assets a WHERE a.platform_id=p.id) AS asset_count
		FROM bastion_platforms p ORDER BY p.builtin DESC, p.category, p.id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	for i := range items {
		items[i].Connectable = validProtocol(items[i].Protocol)
	}
	ok(c, items)
}

func (m *Module) savePlatform(c *gin.Context) {
	var in platformIn
	if !httpx.Bind(c, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Charset == "" {
		in.Charset = "utf-8"
	}
	cat, proto := platformKind(in.Type)
	var p *Platform
	var err error
	if c.Param("id") == "" {
		p, err = queryOne[Platform](c, m, `INSERT INTO bastion_platforms (name,category,type,protocol,port,charset,description)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *, 0::bigint AS asset_count`, in.Name, cat, in.Type, proto, in.Port, in.Charset, in.Description)
	} else {
		id, okk := httpx.ParamID(c, "id")
		if !okk {
			return
		}
		var builtin bool
		if err := m.d.DB.QueryRow(c, `SELECT builtin FROM bastion_platforms WHERE id=$1`, id).Scan(&builtin); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Fail(c, httpx.NotFound("not found"))
				return
			}
			httpx.Fail(c, err)
			return
		}
		if builtin {
			// 内置平台只允许改默认端口、字符集与说明
			p, err = queryOne[Platform](c, m, `UPDATE bastion_platforms SET port=$2,charset=$3,description=$4 WHERE id=$1
				RETURNING *, (SELECT count(*) FROM bastion_assets a WHERE a.platform_id=bastion_platforms.id) AS asset_count`, id, in.Port, in.Charset, in.Description)
		} else {
			p, err = queryOne[Platform](c, m, `UPDATE bastion_platforms SET name=$2,category=$3,type=$4,protocol=$5,port=$6,charset=$7,description=$8 WHERE id=$1
				RETURNING *, (SELECT count(*) FROM bastion_assets a WHERE a.platform_id=bastion_platforms.id) AS asset_count`, id, in.Name, cat, in.Type, proto, in.Port, in.Charset, in.Description)
		}
	}
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	p.Connectable = validProtocol(p.Protocol)
	audit.SetResource(c, "bastion_platform", itoa(p.ID))
	if c.Param("id") == "" {
		httpx.Created(c, p)
		return
	}
	ok(c, p)
}

func (m *Module) deletePlatform(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var builtin bool
	var used int
	err := m.d.DB.QueryRow(c, `SELECT builtin,(SELECT count(*) FROM bastion_assets WHERE platform_id=$1) FROM bastion_platforms WHERE id=$1`, id).Scan(&builtin, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("not found"))
		return
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if builtin {
		httpx.Fail(c, httpx.Conflict("内置平台不能删除"))
		return
	}
	if used > 0 {
		httpx.Fail(c, httpx.Conflict("还有资产使用该平台，请先调整资产").WithDetails(gin.H{"assets": used}))
		return
	}
	if m.exec(c, `DELETE FROM bastion_platforms WHERE id=$1`, id) {
		audit.SetResource(c, "bastion_platform", itoa(id))
		ok(c, gin.H{"id": id})
	}
}

// ---------- 账号连通性验证 ----------

// verifyAccount 用托管凭据真实登录一次（SSH 不打开 Shell；数据库执行 SELECT 1），结果写回账号。
func (m *Module) verifyAccount(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	t, err := m.loadTarget(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if p := rbac.PrincipalFrom(c); p != nil && !p.CanEnv(t.asset.Env) {
		httpx.Fail(c, httpx.Forbidden("environment "+t.asset.Env+" not in your scope"))
		return
	}
	ctx, cancel := context.WithTimeout(c, 15*time.Second)
	defer cancel()
	start := time.Now()
	verr := m.probeAccount(ctx, t)
	status, msg := "ok", "登录成功"
	if verr != nil {
		status, msg = "failed", verr.Error()
	}
	if _, err := m.d.DB.Exec(ctx, `UPDATE bastion_accounts SET verify_status=$2, verify_message=$3, verified_at=now() WHERE id=$1`, id, status, msg); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "bastion_account", itoa(id))
	audit.SetDetail(c, gin.H{"verify": status})
	ok(c, gin.H{"id": id, "status": status, "message": msg, "latency_ms": time.Since(start).Milliseconds()})
}

func (m *Module) probeAccount(ctx context.Context, t *rotationTarget) error {
	switch t.asset.Protocol {
	case "ssh":
		auth, err := m.authMethod(t.acc)
		if err != nil {
			return errors.New("托管凭据无法解密或格式错误")
		}
		addr := net.JoinHostPort(t.asset.Host, strconv.Itoa(t.asset.Port))
		d := net.Dialer{Timeout: 8 * time.Second}
		raw, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return errors.New(dialReason(err))
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
		conn, chans, reqs, err := ssh.NewClientConn(raw, addr, &ssh.ClientConfig{User: t.acc.Username, Auth: []ssh.AuthMethod{auth},
			HostKeyCallback: m.hostKeyCallback(t.asset), Timeout: 8 * time.Second})
		if err != nil {
			if strings.Contains(err.Error(), "host key mismatch") {
				return errors.New("主机指纹与登记的不一致")
			}
			return errors.New("登录失败：账号或凭据不正确")
		}
		ssh.NewClient(conn, chans, reqs).Close()
		return nil
	case "mysql", "postgres":
		db, err := m.openDB(t, "")
		if err != nil {
			return errors.New("无法建立数据库连接")
		}
		defer db.Close()
		var one int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			return errors.New(dbErrReason(err))
		}
		return nil
	}
	return errors.New("该协议没有可用的连接器")
}

func dbErrReason(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "timeout"):
		return "连接超时"
	case strings.Contains(s, "access denied") || strings.Contains(s, "password authentication failed") || strings.Contains(s, "28p01") || strings.Contains(s, "1045"):
		return "登录失败：账号或密码不正确"
	case strings.Contains(s, "refused"):
		return "端口拒绝连接：服务未启动或端口错误"
	case strings.Contains(s, "unknown database") || strings.Contains(s, "does not exist"):
		return "数据库不存在"
	}
	return "数据库连接失败"
}

// ---------- 控制台总览 ----------

func (m *Module) overview(c *gin.Context) {
	loc := aclLocation()
	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	out := gin.H{"as_of": time.Now().UTC(), "day_start": dayStart.UTC()}
	var assets, hosts, dbs, accounts, nodes int64
	var online, todaySessions, todayUsers, loginOK, loginDenied, loginFailed int64
	var rotFailed7d, rotOK7d, verifyFailed, plans int64
	var cmdDenied, dbToday int64
	err := m.d.DB.QueryRow(c, `SELECT
		(SELECT count(*) FROM bastion_assets),
		(SELECT count(*) FROM bastion_assets WHERE protocol='ssh'),
		(SELECT count(*) FROM bastion_assets WHERE protocol IN ('mysql','postgres')),
		(SELECT count(*) FROM bastion_accounts),
		(SELECT count(*) FROM bastion_nodes),
		(SELECT count(*) FROM bastion_sessions WHERE status='active'),
		(SELECT count(*) FROM bastion_sessions WHERE started_at >= $1),
		(SELECT count(DISTINCT user_id) FROM bastion_sessions WHERE started_at >= $1),
		(SELECT count(*) FROM bastion_login_logs WHERE created_at >= $1 AND result='success'),
		(SELECT count(*) FROM bastion_login_logs WHERE created_at >= $1 AND result='denied'),
		(SELECT count(*) FROM bastion_login_logs WHERE created_at >= $1 AND result='failed'),
		(SELECT count(*) FROM bastion_rotation_runs WHERE started_at >= now()-interval '7 days' AND status='failed'),
		(SELECT count(*) FROM bastion_rotation_runs WHERE started_at >= now()-interval '7 days' AND status='succeeded'),
		(SELECT count(*) FROM bastion_accounts WHERE verify_status='failed'),
		(SELECT count(*) FROM bastion_rotation_plans WHERE enabled),
		(SELECT count(*) FROM bastion_commands WHERE created_at >= $1 AND action IN ('deny','rejected')),
		(SELECT count(*) FROM bastion_db_queries WHERE created_at >= $1)`, dayStart).Scan(
		&assets, &hosts, &dbs, &accounts, &nodes, &online, &todaySessions, &todayUsers, &loginOK, &loginDenied, &loginFailed,
		&rotFailed7d, &rotOK7d, &verifyFailed, &plans, &cmdDenied, &dbToday)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var pendingCmd, pendingSQL, pendingAccess int64
	// 待审批数量直接读取平台审批单（类型由本模块注册）
	_ = m.d.DB.QueryRow(c, `SELECT
		count(*) FILTER (WHERE type=$1), count(*) FILTER (WHERE type=$2), count(*) FILTER (WHERE type=$3)
		FROM approval_tickets WHERE status='pending'`, CommandApprovalType, SQLApprovalType, AccessRequestType).Scan(&pendingCmd, &pendingSQL, &pendingAccess)
	out["assets"] = gin.H{"total": assets, "hosts": hosts, "databases": dbs, "accounts": accounts, "nodes": nodes}
	out["sessions"] = gin.H{"online": online, "today": todaySessions, "today_users": todayUsers}
	out["logins"] = gin.H{"success": loginOK, "denied": loginDenied, "failed": loginFailed}
	out["approvals"] = gin.H{"command": pendingCmd, "sql": pendingSQL, "access": pendingAccess}
	out["rotation"] = gin.H{"failed_7d": rotFailed7d, "succeeded_7d": rotOK7d, "enabled_plans": plans, "verify_failed": verifyFailed}
	out["commands"] = gin.H{"blocked_today": cmdDenied}
	out["db_queries_today"] = dbToday

	// 近 7 天每日会话数
	trend := make([]gin.H, 0, 7)
	rows, err := m.d.DB.Query(c, `SELECT d::date, (SELECT count(*) FROM bastion_sessions s WHERE (s.started_at AT TIME ZONE $1)::date = d::date)
		FROM generate_series(($2::timestamptz AT TIME ZONE $1)::date - 6, ($2::timestamptz AT TIME ZONE $1)::date, interval '1 day') d ORDER BY d`, loc.String(), dayStart)
	if err == nil {
		for rows.Next() {
			var d time.Time
			var n int64
			if rows.Scan(&d, &n) == nil {
				trend = append(trend, gin.H{"date": d.Format("2006-01-02"), "sessions": n})
			}
		}
		rows.Close()
	}
	out["trend"] = trend

	// 今日最常访问的资产
	top := []gin.H{}
	rows, err = m.d.DB.Query(c, `SELECT asset_name, count(*) FROM bastion_sessions WHERE started_at >= now()-interval '7 days' GROUP BY asset_name ORDER BY 2 DESC LIMIT 5`)
	if err == nil {
		for rows.Next() {
			var name string
			var n int64
			if rows.Scan(&name, &n) == nil {
				top = append(top, gin.H{"asset": name, "sessions": n})
			}
		}
		rows.Close()
	}
	out["top_assets"] = top
	ok(c, out)
}

// ---------- 全局命令记录 ----------

// CommandRecord 是命令记录列表的一行（含会话上下文）。
type CommandRecord struct {
	ID            int64     `db:"id" json:"id"`
	SessionID     int64     `db:"session_id" json:"session_id"`
	Command       string    `db:"command" json:"command"`
	Action        string    `db:"action" json:"action"`
	RuleID        *int64    `db:"rule_id" json:"rule_id"`
	RuleName      *string   `db:"rule_name" json:"rule_name"`
	DeliveryState string    `db:"delivery_state" json:"delivery_state"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	Username      string    `db:"username" json:"username"`
	AssetID       int64     `db:"asset_id" json:"asset_id"`
	AssetName     string    `db:"asset_name" json:"asset_name"`
	AccountUser   string    `db:"account_user" json:"account_user"`
	SessionStart  time.Time `db:"session_started_at" json:"session_started_at"`
}

func (m *Module) listAllCommands(c *gin.Context) {
	p := httpx.ParsePage(c)
	from, err := httpx.QueryTime(c, "from")
	if err != nil {
		httpx.Fail(c, httpx.Validation("from must be RFC3339"))
		return
	}
	to, err := httpx.QueryTime(c, "to")
	if err != nil {
		httpx.Fail(c, httpx.Validation("to must be RFC3339"))
		return
	}
	q := "%" + escapeLike(strings.TrimSpace(c.Query("q"))) + "%"
	where := `WHERE (c.command ILIKE $1 OR s.username ILIKE $1 OR s.asset_name ILIKE $1)
		AND ($2='' OR c.action=$2) AND ($3='' OR s.username=$3) AND ($4='' OR s.asset_id::text=$4)
		AND ($5::timestamptz IS NULL OR c.created_at >= $5) AND ($6::timestamptz IS NULL OR c.created_at < $6)`
	args := []any{q, c.Query("action"), c.Query("username"), c.Query("asset_id"), nullTime(from), nullTime(to)}
	items, err := queryAll[CommandRecord](c, m, `SELECT c.id,c.session_id,c.command,c.action,c.rule_id,f.name AS rule_name,c.delivery_state,c.created_at,
		s.username,s.asset_id,s.asset_name,s.account_user,s.started_at AS session_started_at
		FROM bastion_commands c JOIN bastion_sessions s ON s.id=c.session_id LEFT JOIN bastion_filters f ON f.id=c.rule_id `+where+
		` ORDER BY c.id DESC LIMIT $7 OFFSET $8`, append(args, p.Limit(), p.Offset())...)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var total int64
	_ = m.d.DB.QueryRow(c, `SELECT count(*) FROM bastion_commands c JOIN bastion_sessions s ON s.id=c.session_id `+where, args...).Scan(&total)
	ok(c, httpx.NewPageResult(items, total, p))
}
