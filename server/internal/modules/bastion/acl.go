package bastion

// 登录 ACL（对应 JumpServer「连接 ACL / 登录 ACL」）：
// 按用户 / 角色 / 用户组、资产、来源 IP、星期与时段控制经堡垒机连接资产（SSH 终端与数据库控制台）。
// 规则按 priority 从小到大匹配，第一条命中的规则决定 allow / deny；没有规则命中时放行（与授权规则叠加，授权仍是必要条件）。
// 每次连接尝试（成功、被拒绝、失败）写入 bastion_login_logs。

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // 容器内可能没有系统时区库，ACL 时段按 MATRIX_TZ（默认 Asia/Shanghai）判断

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// LoginACL 是一条登录 ACL 规则。
type LoginACL struct {
	ID          int64     `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	Priority    int       `db:"priority" json:"priority"`
	SubjectType string    `db:"subject_type" json:"subject_type"`
	Subject     string    `db:"subject" json:"subject"`
	AssetIDs    []int64   `db:"asset_ids" json:"asset_ids"`
	IPRanges    []string  `db:"ip_ranges" json:"ip_ranges"`
	Weekdays    []int     `db:"weekdays" json:"weekdays"`
	TimeFrom    string    `db:"time_from" json:"time_from"`
	TimeTo      string    `db:"time_to" json:"time_to"`
	Action      string    `db:"action" json:"action"`
	Enabled     bool      `db:"enabled" json:"enabled"`
	Description string    `db:"description" json:"description"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// aclLocation 是判断时段所用的时区。
func aclLocation() *time.Location {
	name := os.Getenv("MATRIX_TZ")
	if name == "" {
		name = "Asia/Shanghai"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.Local
}

// parseClock 把 "HH:MM" 转成当天的分钟数。
func parseClock(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[:2])
	mm, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return h*60 + mm, true
}

// ipInRange 支持单个 IP、CIDR 与 "起始IP-结束IP"（同族）。
func ipInRange(ip net.IP, r string) bool {
	r = strings.TrimSpace(r)
	if r == "" || ip == nil {
		return false
	}
	if r == "*" {
		return true
	}
	if strings.Contains(r, "/") {
		_, n, err := net.ParseCIDR(r)
		return err == nil && n.Contains(ip)
	}
	if i := strings.Index(r, "-"); i > 0 {
		lo, hi := net.ParseIP(strings.TrimSpace(r[:i])), net.ParseIP(strings.TrimSpace(r[i+1:]))
		if lo == nil || hi == nil {
			return false
		}
		if v4 := ip.To4(); v4 != nil {
			ip, lo, hi = v4, lo.To4(), hi.To4()
			if lo == nil || hi == nil {
				return false
			}
		} else {
			ip, lo, hi = ip.To16(), lo.To16(), hi.To16()
		}
		return bytesCompare(ip, lo) >= 0 && bytesCompare(ip, hi) <= 0
	}
	want := net.ParseIP(r)
	return want != nil && want.Equal(ip)
}

func bytesCompare(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

func validIPRange(r string) bool {
	r = strings.TrimSpace(r)
	if r == "*" {
		return true
	}
	if strings.Contains(r, "/") {
		_, _, err := net.ParseCIDR(r)
		return err == nil
	}
	if i := strings.Index(r, "-"); i > 0 {
		lo, hi := net.ParseIP(strings.TrimSpace(r[:i])), net.ParseIP(strings.TrimSpace(r[i+1:]))
		return lo != nil && hi != nil && (lo.To4() == nil) == (hi.To4() == nil) && bytesCompare(lo.To16(), hi.To16()) <= 0
	}
	return net.ParseIP(r) != nil
}

// aclSubject 描述被判断的连接方。
type aclSubject struct {
	UserID int64
	Roles  []string
	Groups []string // 用户组 ID（文本）
}

// MatchACL 判断规则是否命中（不看 enabled，由调用方过滤）。
func MatchACL(r *LoginACL, who aclSubject, assetID int64, ip net.IP, now time.Time) bool {
	switch r.SubjectType {
	case "user":
		if r.Subject != strconv.FormatInt(who.UserID, 10) {
			return false
		}
	case "role":
		if !containsStr(who.Roles, r.Subject) {
			return false
		}
	case "group":
		if !containsStr(who.Groups, r.Subject) {
			return false
		}
	}
	if len(r.AssetIDs) > 0 {
		hit := false
		for _, id := range r.AssetIDs {
			hit = hit || id == assetID
		}
		if !hit {
			return false
		}
	}
	if len(r.IPRanges) > 0 {
		hit := false
		for _, rg := range r.IPRanges {
			hit = hit || ipInRange(ip, rg)
		}
		if !hit {
			return false
		}
	}
	if len(r.Weekdays) > 0 {
		wd := int(now.Weekday()) // 0=周日
		if wd == 0 {
			wd = 7
		}
		hit := false
		for _, d := range r.Weekdays {
			hit = hit || d == wd
		}
		if !hit {
			return false
		}
	}
	from, okFrom := parseClock(r.TimeFrom)
	to, okTo := parseClock(r.TimeTo)
	if okFrom && okTo {
		cur := now.Hour()*60 + now.Minute()
		if from <= to {
			if cur < from || cur >= to {
				return false
			}
		} else if cur < from && cur >= to { // 跨午夜，如 22:00-06:00
			return false
		}
	}
	return true
}

// EvaluateACL 返回第一条命中的启用规则（rules 已按优先级排序），没有命中时返回 nil。
func EvaluateACL(rules []LoginACL, who aclSubject, assetID int64, ip net.IP, now time.Time) *LoginACL {
	for i := range rules {
		if rules[i].Enabled && MatchACL(&rules[i], who, assetID, ip, now) {
			return &rules[i]
		}
	}
	return nil
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// checkACL 判断 p 从 clientIP 连接 asset 是否被 ACL 拒绝；返回拒绝原因（空表示放行）。
func (m *Module) checkACL(ctx context.Context, p *rbac.Principal, assetID int64, clientIP string) (string, error) {
	rules, err := queryAll[LoginACL](ctx, m, `SELECT * FROM bastion_login_acls WHERE enabled ORDER BY priority, id`)
	if err != nil {
		return "", err
	}
	if len(rules) == 0 {
		return "", nil
	}
	who := aclSubject{UserID: p.UserID, Roles: roles(p)}
	rows, err := m.d.DB.Query(ctx, `SELECT group_id::text FROM user_group_members WHERE user_id=$1`, p.UserID)
	if err == nil {
		for rows.Next() {
			var g string
			if rows.Scan(&g) == nil {
				who.Groups = append(who.Groups, g)
			}
		}
		rows.Close()
	}
	hit := EvaluateACL(rules, who, assetID, net.ParseIP(clientIP), time.Now().In(aclLocation()))
	if hit != nil && hit.Action == "deny" {
		return fmt.Sprintf("登录 ACL「%s」禁止本次连接", hit.Name), nil
	}
	return "", nil
}

// ---------- 登录日志 ----------

type loginLog struct {
	UserID    int64
	Username  string
	AssetID   int64
	AssetName string
	Account   string
	Protocol  string
	ClientIP  string
	Result    string // success | denied | failed
	Reason    string
	SessionID *int64
}

// writeLoginLog 写一条资产登录日志；失败只记日志，不影响主流程。
func (m *Module) writeLoginLog(l loginLog) {
	if m == nil || m.d == nil || m.d.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), databaseTimeout)
	defer cancel()
	if len(l.Reason) > 500 {
		l.Reason = l.Reason[:500]
	}
	if _, err := m.d.DB.Exec(ctx, `INSERT INTO bastion_login_logs (user_id,username,asset_id,asset_name,account,protocol,client_ip,result,reason,session_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, l.UserID, l.Username, l.AssetID, l.AssetName, l.Account, l.Protocol, l.ClientIP, l.Result, l.Reason, l.SessionID); err != nil && m.d.Logger != nil {
		m.d.Logger.Warn("bastion: login log not persisted", "error", err)
	}
}

// LoginLogEntry 是登录日志接口返回的一行。
type LoginLogEntry struct {
	ID        int64     `db:"id" json:"id"`
	UserID    int64     `db:"user_id" json:"user_id"`
	Username  string    `db:"username" json:"username"`
	AssetID   int64     `db:"asset_id" json:"asset_id"`
	AssetName string    `db:"asset_name" json:"asset_name"`
	Account   string    `db:"account" json:"account"`
	Protocol  string    `db:"protocol" json:"protocol"`
	ClientIP  string    `db:"client_ip" json:"client_ip"`
	Result    string    `db:"result" json:"result"`
	Reason    string    `db:"reason" json:"reason"`
	SessionID *int64    `db:"session_id" json:"session_id"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

func (m *Module) listLoginLogs(c *gin.Context) {
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
	where := `WHERE ($1='' OR result=$1) AND ($2='' OR protocol=$2)
		AND (username ILIKE $3 OR asset_name ILIKE $3 OR client_ip ILIKE $3 OR account ILIKE $3)
		AND ($4::timestamptz IS NULL OR created_at >= $4) AND ($5::timestamptz IS NULL OR created_at < $5)`
	args := []any{c.Query("result"), c.Query("protocol"), q, nullTime(from), nullTime(to)}
	items, err := queryAll[LoginLogEntry](c, m, `SELECT * FROM bastion_login_logs `+where+` ORDER BY id DESC LIMIT $6 OFFSET $7`, append(args, p.Limit(), p.Offset())...)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var total int64
	_ = m.d.DB.QueryRow(c, `SELECT count(*) FROM bastion_login_logs `+where, args...).Scan(&total)
	ok(c, httpx.NewPageResult(items, total, p))
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}

// ---------- ACL HTTP ----------

type aclIn struct {
	Name        string   `json:"name" binding:"required,max=64"`
	Priority    int      `json:"priority"`
	SubjectType string   `json:"subject_type" binding:"required,oneof=all user role group"`
	Subject     string   `json:"subject" binding:"max=160"`
	AssetIDs    []int64  `json:"asset_ids"`
	IPRanges    []string `json:"ip_ranges"`
	Weekdays    []int    `json:"weekdays"`
	TimeFrom    string   `json:"time_from"`
	TimeTo      string   `json:"time_to"`
	Action      string   `json:"action" binding:"required,oneof=allow deny"`
	Enabled     *bool    `json:"enabled"`
	Description string   `json:"description" binding:"max=500"`
}

func (in *aclIn) check() error {
	in.Name, in.Subject = strings.TrimSpace(in.Name), strings.TrimSpace(in.Subject)
	if in.Name == "" {
		return httpx.Validation("name is required")
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	if in.Priority < 1 || in.Priority > 100000 {
		return httpx.Validation("priority must be 1-100000")
	}
	if in.SubjectType == "all" {
		in.Subject = ""
	} else if in.Subject == "" {
		return httpx.Validation("subject is required unless subject_type is all")
	}
	if in.AssetIDs == nil {
		in.AssetIDs = []int64{}
	}
	ranges := []string{}
	for _, r := range in.IPRanges {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !validIPRange(r) {
			return httpx.Validation("invalid ip range: " + r)
		}
		ranges = append(ranges, r)
	}
	if len(ranges) > 100 {
		return httpx.Validation("too many ip ranges (max 100)")
	}
	in.IPRanges = ranges
	if in.Weekdays == nil {
		in.Weekdays = []int{}
	}
	for _, d := range in.Weekdays {
		if d < 1 || d > 7 {
			return httpx.Validation("weekdays must be 1 (Mon) - 7 (Sun)")
		}
	}
	in.TimeFrom, in.TimeTo = strings.TrimSpace(in.TimeFrom), strings.TrimSpace(in.TimeTo)
	if (in.TimeFrom == "") != (in.TimeTo == "") {
		return httpx.Validation("time_from and time_to must be set together")
	}
	if in.TimeFrom != "" {
		f, ok1 := parseClock(in.TimeFrom)
		t, ok2 := parseClock(in.TimeTo)
		if !ok1 || !ok2 {
			return httpx.Validation("time must be HH:MM")
		}
		if f == t {
			return httpx.Validation("time_from and time_to must differ")
		}
	}
	return nil
}

func (m *Module) listACLs(c *gin.Context) {
	items, err := queryAll[LoginACL](c, m, `SELECT * FROM bastion_login_acls ORDER BY priority, id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, items)
}

func (m *Module) saveACL(c *gin.Context) {
	var in aclIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	en := in.Enabled == nil || *in.Enabled
	var r *LoginACL
	var err error
	if c.Param("id") == "" {
		r, err = queryOne[LoginACL](c, m, `INSERT INTO bastion_login_acls (name,priority,subject_type,subject,asset_ids,ip_ranges,weekdays,time_from,time_to,action,enabled,description)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING *`, in.Name, in.Priority, in.SubjectType, in.Subject, in.AssetIDs, in.IPRanges, in.Weekdays,
			in.TimeFrom, in.TimeTo, in.Action, en, in.Description)
	} else {
		id, okk := httpx.ParamID(c, "id")
		if !okk {
			return
		}
		r, err = queryOne[LoginACL](c, m, `UPDATE bastion_login_acls SET name=$2,priority=$3,subject_type=$4,subject=$5,asset_ids=$6,ip_ranges=$7,weekdays=$8,
			time_from=$9,time_to=$10,action=$11,enabled=$12,description=$13,updated_at=now() WHERE id=$1 RETURNING *`, id, in.Name, in.Priority, in.SubjectType, in.Subject,
			in.AssetIDs, in.IPRanges, in.Weekdays, in.TimeFrom, in.TimeTo, in.Action, en, in.Description)
	}
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_login_acl", itoa(r.ID))
	if c.Param("id") == "" {
		httpx.Created(c, r)
		return
	}
	ok(c, r)
}

func (m *Module) deleteACL(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if okk && m.exec(c, `DELETE FROM bastion_login_acls WHERE id=$1`, id) {
		audit.SetResource(c, "bastion_login_acl", itoa(id))
		ok(c, gin.H{"id": id})
	}
}
