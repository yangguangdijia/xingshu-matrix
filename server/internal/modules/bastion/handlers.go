package bastion

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

var ok = httpx.OK

// ---------- models ----------

type Asset struct {
	ID          int64          `db:"id" json:"id"`
	Name        string         `db:"name" json:"name"`
	Host        string         `db:"host" json:"host"`
	Port        int            `db:"port" json:"port"`
	Protocol    string         `db:"protocol" json:"protocol"`
	Env         string         `db:"env" json:"env"`
	Description string         `db:"description" json:"description"`
	HostKey     string         `db:"host_key" json:"host_key"`
	Options     map[string]any `db:"options" json:"options"` // 协议专属非敏感参数
	CreatedAt   time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time      `db:"updated_at" json:"updated_at"`
}

type Account struct {
	ID         int64     `db:"id" json:"id"`
	AssetID    int64     `db:"asset_id" json:"asset_id"`
	Name       string    `db:"name" json:"name"`
	Username   string    `db:"username" json:"username"`
	AuthType   string    `db:"auth_type" json:"auth_type"`
	Secret     string    `db:"secret_enc" json:"secret"` // masked in responses
	Passphrase string    `db:"passphrase_enc" json:"-"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time `db:"updated_at" json:"updated_at"`
}

type Rule struct {
	ID          int64      `db:"id" json:"id"`
	Name        string     `db:"name" json:"name"`
	SubjectType string     `db:"subject_type" json:"subject_type"`
	Subject     string     `db:"subject" json:"subject"`
	AssetIDs    []int64    `db:"asset_ids" json:"asset_ids"`
	AccountIDs  []int64    `db:"account_ids" json:"account_ids"`
	ExpiresAt   *time.Time `db:"expires_at" json:"expires_at"`
	Enabled     bool       `db:"enabled" json:"enabled"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
}

type Filter struct {
	ID        int64     `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	Pattern   string    `db:"pattern" json:"pattern"`
	Action    string    `db:"action" json:"action"`
	Priority  int       `db:"priority" json:"priority"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type Session struct {
	ID            int64      `db:"id" json:"id"`
	UserID        int64      `db:"user_id" json:"user_id"`
	Username      string     `db:"username" json:"username"`
	AssetID       int64      `db:"asset_id" json:"asset_id"`
	AssetName     string     `db:"asset_name" json:"asset_name"`
	AccountID     int64      `db:"account_id" json:"account_id"`
	AccountUser   string     `db:"account_user" json:"account_user"`
	Protocol      string     `db:"protocol" json:"protocol"`
	ClientIP      string     `db:"client_ip" json:"client_ip"`
	Status        string     `db:"status" json:"status"`
	Error         string     `db:"error" json:"error"`
	RecordingPath string     `db:"recording_path" json:"-"`
	StartedAt     time.Time  `db:"started_at" json:"started_at"`
	EndedAt       *time.Time `db:"ended_at" json:"ended_at"`
	HasRecording  bool       `db:"-" json:"has_recording"`
}

type Command struct {
	ID        int64     `db:"id" json:"id"`
	SessionID int64     `db:"session_id" json:"session_id"`
	Command   string    `db:"command" json:"command"`
	Action    string    `db:"action" json:"action"`
	RuleID    *int64    `db:"rule_id" json:"rule_id"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

func queryAll[T any](ctx context.Context, m *Module, sql string, args ...any) ([]T, error) {
	rows, err := m.d.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByNameLax[T])
}

func queryOne[T any](ctx context.Context, m *Module, sql string, args ...any) (*T, error) {
	rows, err := m.d.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByNameLax[T])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("not found")
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (m *Module) exec(c *gin.Context, sql string, args ...any) bool {
	tag, err := m.d.DB.Exec(c, sql, args...)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return false
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("not found"))
		return false
	}
	return true
}

func mapPgErr(err error) error {
	if s := err.Error(); strings.Contains(s, "SQLSTATE 23505") {
		return httpx.Conflict("already exists")
	} else if strings.Contains(s, "SQLSTATE 23") {
		return httpx.BadRequest(s)
	}
	return err
}

// ---------- assets ----------

type assetIn struct {
	Name        string         `json:"name" binding:"required"`
	Host        string         `json:"host" binding:"required"`
	Port        int            `json:"port" binding:"required,min=1,max=65535"`
	Protocol    string         `json:"protocol" binding:"required"`
	Env         string         `json:"env"`
	Description string         `json:"description"`
	HostKey     string         `json:"host_key"`
	Options     map[string]any `json:"options"`
}

// sensitiveOptKeys 是绝不能进入 options 凭据字段的关键字（大小写不敏感的子串匹配）。
var sensitiveOptKeys = []string{"secret", "password", "passphrase", "private_key", "privatekey", "token", "kubeconfig", "credential"}

func (in *assetIn) check() error {
	in.Protocol = strings.ToLower(in.Protocol)
	if !validProtocol(in.Protocol) {
		return httpx.Validation("protocol must be ssh|rdp|vnc|mysql|postgres")
	}
	if in.Env == "" {
		in.Env = "dev"
	}
	if in.Options == nil {
		in.Options = map[string]any{}
	}
	for k := range in.Options {
		lk := strings.ToLower(k)
		for _, s := range sensitiveOptKeys {
			if strings.Contains(lk, s) {
				return httpx.Validation("options must not contain credentials: " + k)
			}
		}
	}
	b, err := json.Marshal(in.Options)
	if err != nil {
		return httpx.Validation("options must be a JSON object")
	}
	if len(b) > 4096 {
		return httpx.Validation("options is too large (max 4096 bytes)")
	}
	return nil
}

func (m *Module) listAssets(c *gin.Context) {
	p := httpx.ParsePage(c)
	q, proto, env := "%"+c.Query("q")+"%", c.Query("protocol"), c.Query("env")
	where := `WHERE (name ILIKE $1 OR host ILIKE $1) AND ($2='' OR protocol=$2) AND ($3='' OR env=$3)`
	items, err := queryAll[Asset](c, m, `SELECT * FROM bastion_assets `+where+` ORDER BY id DESC LIMIT $4 OFFSET $5`, q, proto, env, p.Limit(), p.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var total int64
	_ = m.d.DB.QueryRow(c, `SELECT count(*) FROM bastion_assets `+where, q, proto, env).Scan(&total)
	ok(c, httpx.NewPageResult(items, total, p))
}

func (m *Module) getAsset(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	a, err := queryOne[Asset](c, m, `SELECT * FROM bastion_assets WHERE id=$1`, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, a)
}

func (m *Module) createAsset(c *gin.Context) {
	var in assetIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	a, err := queryOne[Asset](c, m, `INSERT INTO bastion_assets (name,host,port,protocol,env,description,host_key,options)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING *`, in.Name, in.Host, in.Port, in.Protocol, in.Env, in.Description, strings.TrimSpace(in.HostKey), in.Options)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_asset", itoa(a.ID))
	httpx.Created(c, a)
}

func (m *Module) updateAsset(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var in assetIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	a, err := queryOne[Asset](c, m, `UPDATE bastion_assets SET name=$2,host=$3,port=$4,protocol=$5,env=$6,description=$7,host_key=$8,options=$9,updated_at=now()
		WHERE id=$1 RETURNING *`, id, in.Name, in.Host, in.Port, in.Protocol, in.Env, in.Description, strings.TrimSpace(in.HostKey), in.Options)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_asset", itoa(id))
	ok(c, a)
}

func (m *Module) deleteAsset(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if okk && m.exec(c, `DELETE FROM bastion_assets WHERE id=$1`, id) {
		audit.SetResource(c, "bastion_asset", itoa(id))
		ok(c, gin.H{"id": id})
	}
}

// ---------- accounts ----------

type accountIn struct {
	AssetID    int64  `json:"asset_id" binding:"required"`
	Name       string `json:"name" binding:"required"`
	Username   string `json:"username" binding:"required"`
	AuthType   string `json:"auth_type" binding:"required,oneof=password private_key"`
	Secret     string `json:"secret"`     // password or PEM private key; empty on update keeps current
	Passphrase string `json:"passphrase"` // optional private key passphrase
}

func maskAccount(a *Account) {
	a.Secret = crypto.Mask(a.Secret)
}

func (m *Module) listAccounts(c *gin.Context) {
	assetID := c.Query("asset_id")
	items, err := queryAll[Account](c, m, `SELECT * FROM bastion_accounts WHERE ($1='' OR asset_id::text=$1) ORDER BY id`, assetID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	for i := range items {
		maskAccount(&items[i])
	}
	ok(c, items)
}

func (m *Module) encSecrets(in *accountIn) (sec, pass string, err error) {
	if in.Secret != "" {
		if sec, err = m.d.Crypto.Encrypt(in.Secret); err != nil {
			return
		}
	}
	if in.Passphrase != "" {
		pass, err = m.d.Crypto.Encrypt(in.Passphrase)
	}
	return
}

func (m *Module) createAccount(c *gin.Context) {
	var in accountIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Secret == "" {
		httpx.Fail(c, httpx.Validation("secret is required"))
		return
	}
	if in.AuthType == "private_key" {
		if _, err := parseSigner(in.Secret, in.Passphrase); err != nil {
			httpx.Fail(c, httpx.Validation("invalid private key: "+err.Error()))
			return
		}
	}
	sec, pass, err := m.encSecrets(&in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	a, err := queryOne[Account](c, m, `INSERT INTO bastion_accounts (asset_id,name,username,auth_type,secret_enc,passphrase_enc)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING *`, in.AssetID, in.Name, in.Username, in.AuthType, sec, pass)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_account", itoa(a.ID))
	maskAccount(a)
	httpx.Created(c, a)
}

func (m *Module) updateAccount(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var in accountIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.AuthType == "private_key" && in.Secret != "" {
		if _, err := parseSigner(in.Secret, in.Passphrase); err != nil {
			httpx.Fail(c, httpx.Validation("invalid private key: "+err.Error()))
			return
		}
	}
	sec, pass, err := m.encSecrets(&in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	a, err := queryOne[Account](c, m, `UPDATE bastion_accounts SET asset_id=$2,name=$3,username=$4,auth_type=$5,
		secret_enc=COALESCE(NULLIF($6,''),secret_enc), passphrase_enc=CASE WHEN $6<>'' THEN $7 ELSE passphrase_enc END, updated_at=now()
		WHERE id=$1 RETURNING *`, id, in.AssetID, in.Name, in.Username, in.AuthType, sec, pass)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_account", itoa(id))
	maskAccount(a)
	ok(c, a)
}

func (m *Module) deleteAccount(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if okk && m.exec(c, `DELETE FROM bastion_accounts WHERE id=$1`, id) {
		audit.SetResource(c, "bastion_account", itoa(id))
		ok(c, gin.H{"id": id})
	}
}

// ---------- authorization rules ----------

type ruleIn struct {
	Name        string     `json:"name" binding:"required"`
	SubjectType string     `json:"subject_type" binding:"required,oneof=user role"`
	Subject     string     `json:"subject" binding:"required"`
	AssetIDs    []int64    `json:"asset_ids" binding:"required,min=1"`
	AccountIDs  []int64    `json:"account_ids"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Enabled     *bool      `json:"enabled"`
}

func (in *ruleIn) enabled() bool { return in.Enabled == nil || *in.Enabled }

func (m *Module) listRules(c *gin.Context) {
	items, err := queryAll[Rule](c, m, `SELECT * FROM bastion_rules ORDER BY id DESC`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, items)
}

func (m *Module) createRule(c *gin.Context) {
	var in ruleIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.AccountIDs == nil {
		in.AccountIDs = []int64{}
	}
	r, err := queryOne[Rule](c, m, `INSERT INTO bastion_rules (name,subject_type,subject,asset_ids,account_ids,expires_at,enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *`, in.Name, in.SubjectType, in.Subject, in.AssetIDs, in.AccountIDs, in.ExpiresAt, in.enabled())
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_rule", itoa(r.ID))
	httpx.Created(c, r)
}

func (m *Module) updateRule(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var in ruleIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.AccountIDs == nil {
		in.AccountIDs = []int64{}
	}
	r, err := queryOne[Rule](c, m, `UPDATE bastion_rules SET name=$2,subject_type=$3,subject=$4,asset_ids=$5,account_ids=$6,expires_at=$7,enabled=$8
		WHERE id=$1 RETURNING *`, id, in.Name, in.SubjectType, in.Subject, in.AssetIDs, in.AccountIDs, in.ExpiresAt, in.enabled())
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_rule", itoa(id))
	ok(c, r)
}

func (m *Module) deleteRule(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if okk && m.exec(c, `DELETE FROM bastion_rules WHERE id=$1`, id) {
		audit.SetResource(c, "bastion_rule", itoa(id))
		ok(c, gin.H{"id": id})
	}
}

const grantSQL = `SELECT DISTINCT a.id AS asset_id, a.name AS asset_name, a.host, a.port, a.protocol, a.env,
	acc.id AS account_id, acc.name AS account_name, acc.username
  FROM bastion_rules r
  JOIN bastion_assets a ON a.id = ANY(r.asset_ids)
  JOIN bastion_accounts acc ON acc.asset_id = a.id AND (cardinality(r.account_ids)=0 OR acc.id = ANY(r.account_ids))
 WHERE r.enabled AND (r.expires_at IS NULL OR r.expires_at > now())
   AND ((r.subject_type='user' AND r.subject=$1) OR (r.subject_type='role' AND r.subject = ANY($2)))`

type Grant struct {
	AssetID     int64  `db:"asset_id" json:"asset_id"`
	AssetName   string `db:"asset_name" json:"asset_name"`
	Host        string `db:"host" json:"host"`
	Port        int    `db:"port" json:"port"`
	Protocol    string `db:"protocol" json:"protocol"`
	Env         string `db:"env" json:"env"`
	AccountID   int64  `db:"account_id" json:"account_id"`
	AccountName string `db:"account_name" json:"account_name"`
	Username    string `db:"username" json:"username"`
}

func roles(p *rbac.Principal) []string {
	if p.Roles == nil {
		return []string{}
	}
	return p.Roles
}

func (m *Module) myAssets(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	items, err := queryAll[Grant](c, m, grantSQL+` ORDER BY a.name, acc.name`, itoa(p.UserID), roles(p))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := items[:0]
	for _, g := range items {
		if p.CanEnv(g.Env) {
			out = append(out, g)
		}
	}
	ok(c, out)
}

// authorized reports whether p may use account on asset via an active rule.
func (m *Module) authorized(ctx context.Context, p *rbac.Principal, assetID, accountID int64) (bool, error) {
	var n int
	err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM (`+grantSQL+`) g WHERE g.asset_id=$3 AND g.account_id=$4`,
		itoa(p.UserID), roles(p), assetID, accountID).Scan(&n)
	return n > 0, err
}

// ---------- command filters ----------

type filterIn struct {
	Name     string `json:"name" binding:"required"`
	Pattern  string `json:"pattern" binding:"required"`
	Action   string `json:"action" binding:"required,oneof=deny confirm"`
	Priority int    `json:"priority"`
	Enabled  *bool  `json:"enabled"`
}

func (in *filterIn) check() error {
	if _, err := regexp.Compile(in.Pattern); err != nil {
		return httpx.Validation("invalid regex: " + err.Error())
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	return nil
}

func (m *Module) listFilters(c *gin.Context) {
	items, err := queryAll[Filter](c, m, `SELECT * FROM bastion_filters ORDER BY priority, id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, items)
}

func (m *Module) createFilter(c *gin.Context) {
	var in filterIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	f, err := queryOne[Filter](c, m, `INSERT INTO bastion_filters (name,pattern,action,priority,enabled) VALUES ($1,$2,$3,$4,$5) RETURNING *`,
		in.Name, in.Pattern, in.Action, in.Priority, in.Enabled == nil || *in.Enabled)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_filter", itoa(f.ID))
	httpx.Created(c, f)
}

func (m *Module) updateFilter(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var in filterIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	f, err := queryOne[Filter](c, m, `UPDATE bastion_filters SET name=$2,pattern=$3,action=$4,priority=$5,enabled=$6 WHERE id=$1 RETURNING *`,
		id, in.Name, in.Pattern, in.Action, in.Priority, in.Enabled == nil || *in.Enabled)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_filter", itoa(id))
	ok(c, f)
}

func (m *Module) deleteFilter(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if okk && m.exec(c, `DELETE FROM bastion_filters WHERE id=$1`, id) {
		audit.SetResource(c, "bastion_filter", itoa(id))
		ok(c, gin.H{"id": id})
	}
}

func (m *Module) loadFilters(ctx context.Context) ([]FilterRule, error) {
	fs, err := queryAll[Filter](ctx, m, `SELECT * FROM bastion_filters WHERE enabled ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	out := make([]FilterRule, 0, len(fs))
	for _, f := range fs {
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			m.d.Logger.Warn("bastion: skip invalid filter", "id", f.ID, "err", err)
			continue
		}
		out = append(out, FilterRule{ID: f.ID, Name: f.Name, Re: re, Action: f.Action})
	}
	return out, nil
}

// ---------- sessions ----------

func (m *Module) listSessions(c *gin.Context) {
	p := httpx.ParsePage(c)
	status, user, asset := c.Query("status"), c.Query("username"), c.Query("asset_id")
	where := `WHERE ($1='' OR status=$1) AND ($2='' OR username=$2) AND ($3='' OR asset_id::text=$3)`
	items, err := queryAll[Session](c, m, `SELECT * FROM bastion_sessions `+where+` ORDER BY id DESC LIMIT $4 OFFSET $5`, status, user, asset, p.Limit(), p.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	for i := range items {
		items[i].HasRecording = items[i].RecordingPath != ""
	}
	var total int64
	_ = m.d.DB.QueryRow(c, `SELECT count(*) FROM bastion_sessions `+where, status, user, asset).Scan(&total)
	ok(c, httpx.NewPageResult(items, total, p))
}

func (m *Module) getSession(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	s, err := queryOne[Session](c, m, `SELECT * FROM bastion_sessions WHERE id=$1`, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	s.HasRecording = s.RecordingPath != ""
	cmds, err := queryAll[Command](c, m, `SELECT * FROM bastion_commands WHERE session_id=$1 ORDER BY id LIMIT 500`, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, gin.H{"session": s, "commands": cmds})
}

func (m *Module) listCommands(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	p := httpx.ParsePage(c)
	items, err := queryAll[Command](c, m, `SELECT * FROM bastion_commands WHERE session_id=$1 ORDER BY id LIMIT $2 OFFSET $3`, id, p.Limit(), p.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var total int64
	_ = m.d.DB.QueryRow(c, `SELECT count(*) FROM bastion_commands WHERE session_id=$1`, id).Scan(&total)
	ok(c, httpx.NewPageResult(items, total, p))
}

func (m *Module) downloadRecording(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	s, err := queryOne[Session](c, m, `SELECT * FROM bastion_sessions WHERE id=$1`, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if s.RecordingPath == "" {
		httpx.Fail(c, httpx.NotFound("no recording"))
		return
	}
	path := filepath.Join(m.recordDir, filepath.Base(s.RecordingPath))
	if _, err := os.Stat(path); err != nil {
		httpx.Fail(c, httpx.NotFound("recording file missing"))
		return
	}
	c.Header("Content-Type", "application/x-asciicast")
	c.FileAttachment(path, filepath.Base(path))
}

func (m *Module) killSession(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	audit.SetResource(c, "bastion_session", itoa(id))
	if !m.kill(id) {
		httpx.Fail(c, httpx.NotFound("session not active on this server instance"))
		return
	}
	ok(c, gin.H{"id": id, "killed": true})
}
