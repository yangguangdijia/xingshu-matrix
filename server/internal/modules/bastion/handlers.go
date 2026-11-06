package bastion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	NodeID      *int64         `db:"node_id" json:"node_id"`
	PlatformID  *int64         `db:"platform_id" json:"platform_id"`
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
	// 改密：待生效的新密码（加密）只在改密流程内部使用，从不返回
	PendingSecret     string     `db:"pending_secret_enc" json:"-"`
	PasswordChangedAt *time.Time `db:"password_changed_at" json:"password_changed_at"`
	// 连通性验证
	VerifyStatus  string     `db:"verify_status" json:"verify_status"`
	VerifyMessage string     `db:"verify_message" json:"verify_message"`
	VerifiedAt    *time.Time `db:"verified_at" json:"verified_at"`
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
	NodeIDs     []int64    `db:"node_ids" json:"node_ids"`
	Actions     []string   `db:"actions" json:"actions"`
	StartsAt    *time.Time `db:"starts_at" json:"starts_at"`
}

type Filter struct {
	ID        int64     `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	Pattern   string    `db:"pattern" json:"pattern"`
	Action    string    `db:"action" json:"action"`
	Priority  int       `db:"priority" json:"priority"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	GroupName string    `db:"group_name" json:"group_name"`
	AssetIDs  []int64   `db:"asset_ids" json:"asset_ids"`
}

type Session struct {
	ID                 int64      `db:"id" json:"id"`
	UserID             int64      `db:"user_id" json:"user_id"`
	Username           string     `db:"username" json:"username"`
	AssetID            int64      `db:"asset_id" json:"asset_id"`
	AssetName          string     `db:"asset_name" json:"asset_name"`
	AccountID          int64      `db:"account_id" json:"account_id"`
	AccountUser        string     `db:"account_user" json:"account_user"`
	Protocol           string     `db:"protocol" json:"protocol"`
	ClientIP           string     `db:"client_ip" json:"client_ip"`
	Status             string     `db:"status" json:"status"`
	Error              string     `db:"error" json:"error"`
	RecordingPath      string     `db:"recording_path" json:"-"`
	StartedAt          time.Time  `db:"started_at" json:"started_at"`
	EndedAt            *time.Time `db:"ended_at" json:"ended_at"`
	HasRecording       bool       `db:"-" json:"has_recording"`
	OwnerID            string     `db:"owner_id" json:"owner_id"`
	FenceToken         string     `db:"fence_token" json:"-"`
	HeartbeatAt        *time.Time `db:"heartbeat_at" json:"heartbeat_at"`
	LeaseExpiresAt     *time.Time `db:"lease_expires_at" json:"lease_expires_at"`
	KillRequestID      string     `db:"kill_request_id" json:"kill_request_id"`
	KillRequestedAt    *time.Time `db:"kill_requested_at" json:"kill_requested_at"`
	KillRequestedBy    *int64     `db:"kill_requested_by" json:"kill_requested_by"`
	KillAcknowledgedAt *time.Time `db:"kill_acknowledged_at" json:"kill_acknowledged_at"`
	RecordingState     string     `db:"recording_state" json:"recording_state"`
	RecordingStorageID string     `db:"recording_storage_id" json:"-"`
}

type Command struct {
	ID            int64      `db:"id" json:"id"`
	SessionID     int64      `db:"session_id" json:"session_id"`
	Command       string     `db:"command" json:"command"`
	Action        string     `db:"action" json:"action"`
	RuleID        *int64     `db:"rule_id" json:"rule_id"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	DeliveryState string     `db:"delivery_state" json:"delivery_state"`
	DeliveryError string     `db:"delivery_error" json:"delivery_error"`
	ForwardedAt   *time.Time `db:"forwarded_at" json:"forwarded_at"`
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
	NodeID      *int64         `json:"node_id"`
	PlatformID  *int64         `json:"platform_id"`
}

// sensitiveOptKeys 是绝不能进入 options 凭据字段的关键字（大小写不敏感的子串匹配）。
var sensitiveOptKeys = []string{"secret", "password", "passphrase", "private_key", "privatekey", "token", "kubeconfig", "credential"}

func (in *assetIn) check() error {
	in.Protocol = strings.ToLower(in.Protocol)
	if !validProtocol(in.Protocol) {
		return httpx.Validation("protocol must be ssh, mysql or postgres; other protocols have no connector")
	}
	if in.NodeID != nil && *in.NodeID <= 0 {
		in.NodeID = nil
	}
	if in.PlatformID != nil && *in.PlatformID <= 0 {
		in.PlatformID = nil
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
	q, proto, env, node := "%"+c.Query("q")+"%", c.Query("protocol"), c.Query("env"), c.Query("node_id")
	// node_id 包含其全部子节点下的资产
	where := `WHERE (name ILIKE $1 OR host ILIKE $1) AND ($2='' OR protocol=$2) AND ($3='' OR env=$3)
	  AND ($4='' OR node_id IN (WITH RECURSIVE t(id) AS (SELECT id FROM bastion_nodes WHERE id::text=$4
	       UNION SELECT n.id FROM bastion_nodes n JOIN t ON n.parent_id=t.id) SELECT id FROM t))`
	items, err := queryAll[Asset](c, m, `SELECT * FROM bastion_assets `+where+` ORDER BY id DESC LIMIT $5 OFFSET $6`, q, proto, env, node, p.Limit(), p.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var total int64
	_ = m.d.DB.QueryRow(c, `SELECT count(*) FROM bastion_assets `+where, q, proto, env, node).Scan(&total)
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
	a, err := queryOne[Asset](c, m, `INSERT INTO bastion_assets (name,host,port,protocol,env,description,host_key,options,node_id,platform_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING *`, in.Name, in.Host, in.Port, in.Protocol, in.Env, in.Description, strings.TrimSpace(in.HostKey), in.Options, in.NodeID, in.PlatformID)
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
	a, err := queryOne[Asset](c, m, `UPDATE bastion_assets SET name=$2,host=$3,port=$4,protocol=$5,env=$6,description=$7,host_key=$8,options=$9,node_id=$10,platform_id=$11,updated_at=now()
		WHERE id=$1 RETURNING *`, id, in.Name, in.Host, in.Port, in.Protocol, in.Env, in.Description, strings.TrimSpace(in.HostKey), in.Options, in.NodeID, in.PlatformID)
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

// ---------- authorization rules (资产授权) ----------

// GrantActions 是授权可包含的动作。connect 由后端强制；upload/download 需要文件传输通道（未实现，仅登记）；
// copy/paste 由 Web 终端前端执行（服务端无法区分粘贴与键入）。
var GrantActions = []string{"connect", "upload", "download", "copy", "paste"}

type ruleIn struct {
	Name        string     `json:"name" binding:"required,max=120"`
	SubjectType string     `json:"subject_type" binding:"required,oneof=user role group"`
	Subject     string     `json:"subject" binding:"required,max=160"`
	AssetIDs    []int64    `json:"asset_ids"`
	NodeIDs     []int64    `json:"node_ids"`
	AccountIDs  []int64    `json:"account_ids"`
	Actions     []string   `json:"actions"`
	StartsAt    *time.Time `json:"starts_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Enabled     *bool      `json:"enabled"`
}

func (in *ruleIn) enabled() bool { return in.Enabled == nil || *in.Enabled }

func (in *ruleIn) check() error {
	in.Name, in.Subject = strings.TrimSpace(in.Name), strings.TrimSpace(in.Subject)
	if in.Name == "" || in.Subject == "" {
		return httpx.Validation("name and subject are required")
	}
	if in.AssetIDs == nil {
		in.AssetIDs = []int64{}
	}
	if in.NodeIDs == nil {
		in.NodeIDs = []int64{}
	}
	if in.AccountIDs == nil {
		in.AccountIDs = []int64{}
	}
	if len(in.AssetIDs)+len(in.NodeIDs) == 0 {
		return httpx.Validation("select at least one asset or node")
	}
	if in.Actions == nil {
		in.Actions = []string{"connect", "upload", "download", "copy", "paste"}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, a := range in.Actions {
		valid := false
		for _, g := range GrantActions {
			valid = valid || a == g
		}
		if !valid {
			return httpx.Validation("unknown action: " + a)
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	in.Actions = out
	if in.StartsAt != nil && in.ExpiresAt != nil && !in.ExpiresAt.After(*in.StartsAt) {
		return httpx.Validation("expires_at must be later than starts_at")
	}
	return nil
}

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
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	r, err := queryOne[Rule](c, m, `INSERT INTO bastion_rules (name,subject_type,subject,asset_ids,account_ids,expires_at,enabled,node_ids,actions,starts_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING *`, in.Name, in.SubjectType, in.Subject, in.AssetIDs, in.AccountIDs, in.ExpiresAt, in.enabled(),
		in.NodeIDs, in.Actions, in.StartsAt)
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
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	r, err := queryOne[Rule](c, m, `UPDATE bastion_rules SET name=$2,subject_type=$3,subject=$4,asset_ids=$5,account_ids=$6,expires_at=$7,enabled=$8,
		node_ids=$9,actions=$10,starts_at=$11 WHERE id=$1 RETURNING *`, id, in.Name, in.SubjectType, in.Subject, in.AssetIDs, in.AccountIDs, in.ExpiresAt, in.enabled(),
		in.NodeIDs, in.Actions, in.StartsAt)
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

// grantSQL 返回当前用户（$1 用户 ID 文本，$2 角色列表）经有效授权可用的「资产 × 账号 × 动作」行，
// $3 为允许的协议列表。资产可直接授权，也可通过节点（含全部子节点）授权；主体可为用户、角色或用户组。
const grantSQL = `WITH RECURSIVE rn(rule_id, node_id) AS (
	SELECT r.id, u.node_id FROM bastion_rules r, unnest(r.node_ids) AS u(node_id)
	UNION
	SELECT rn.rule_id, c.id FROM rn JOIN bastion_nodes c ON c.parent_id = rn.node_id
)
SELECT a.id AS asset_id, a.name AS asset_name, a.host, a.port, a.protocol, a.env,
	acc.id AS account_id, acc.name AS account_name, acc.username, r.actions, r.expires_at
  FROM bastion_rules r
  JOIN bastion_assets a ON (a.id = ANY(r.asset_ids) OR EXISTS (SELECT 1 FROM rn WHERE rn.rule_id = r.id AND rn.node_id = a.node_id))
  JOIN bastion_accounts acc ON acc.asset_id = a.id AND (cardinality(r.account_ids)=0 OR acc.id = ANY(r.account_ids))
 WHERE r.enabled AND (r.expires_at IS NULL OR r.expires_at > now()) AND (r.starts_at IS NULL OR r.starts_at <= now())
   AND a.protocol = ANY($3)
   AND ((r.subject_type='user' AND r.subject=$1) OR (r.subject_type='role' AND r.subject = ANY($2))
     OR (r.subject_type='group' AND r.subject IN (SELECT group_id::text FROM user_group_members WHERE user_id::text=$1)))`

type grantRow struct {
	AssetID     int64      `db:"asset_id"`
	AssetName   string     `db:"asset_name"`
	Host        string     `db:"host"`
	Port        int        `db:"port"`
	Protocol    string     `db:"protocol"`
	Env         string     `db:"env"`
	AccountID   int64      `db:"account_id"`
	AccountName string     `db:"account_name"`
	Username    string     `db:"username"`
	Actions     []string   `db:"actions"`
	ExpiresAt   *time.Time `db:"expires_at"`
}

// Grant 是「我的资产」中的一行：同一资产账号被多条授权覆盖时合并动作、取最晚到期。
type Grant struct {
	AssetID     int64      `json:"asset_id"`
	AssetName   string     `json:"asset_name"`
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	Protocol    string     `json:"protocol"`
	Env         string     `json:"env"`
	AccountID   int64      `json:"account_id"`
	AccountName string     `json:"account_name"`
	Username    string     `json:"username"`
	Actions     []string   `json:"actions"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// mergeGrants 合并重复的资产账号授权，保持 GrantActions 的顺序；只保留含 connect 的结果。
func mergeGrants(rows []grantRow) []Grant {
	idx := map[[2]int64]int{}
	acts := []map[string]bool{}
	forever := []bool{}
	out := []Grant{}
	for _, r := range rows {
		k := [2]int64{r.AssetID, r.AccountID}
		i, seen := idx[k]
		if !seen {
			i = len(out)
			idx[k] = i
			out = append(out, Grant{AssetID: r.AssetID, AssetName: r.AssetName, Host: r.Host, Port: r.Port, Protocol: r.Protocol, Env: r.Env,
				AccountID: r.AccountID, AccountName: r.AccountName, Username: r.Username})
			acts = append(acts, map[string]bool{})
			forever = append(forever, false)
		}
		for _, a := range r.Actions {
			acts[i][a] = true
		}
		if r.ExpiresAt == nil {
			forever[i] = true
		} else if out[i].ExpiresAt == nil || r.ExpiresAt.After(*out[i].ExpiresAt) {
			t := *r.ExpiresAt
			out[i].ExpiresAt = &t
		}
	}
	res := out[:0]
	for i := range out {
		if forever[i] {
			out[i].ExpiresAt = nil
		}
		out[i].Actions = []string{}
		for _, a := range GrantActions {
			if acts[i][a] {
				out[i].Actions = append(out[i].Actions, a)
			}
		}
		if acts[i]["connect"] {
			res = append(res, out[i])
		}
	}
	return res
}

func roles(p *rbac.Principal) []string {
	if p.Roles == nil {
		return []string{}
	}
	return p.Roles
}

var (
	sshProtocols = []string{"ssh"}
	dbProtocols  = []string{"mysql", "postgres"}
)

func (m *Module) grantsFor(ctx context.Context, p *rbac.Principal, protocols []string) ([]Grant, error) {
	rows, err := queryAll[grantRow](ctx, m, grantSQL+` ORDER BY a.name, acc.name`, itoa(p.UserID), roles(p), protocols)
	if err != nil {
		return nil, err
	}
	all := mergeGrants(rows)
	out := all[:0]
	for _, g := range all {
		if p.CanEnv(g.Env) {
			out = append(out, g)
		}
	}
	return out, nil
}

// myAssets 返回当前用户可连接的 SSH 资产账号（Web 终端使用）。
func (m *Module) myAssets(c *gin.Context) {
	items, err := m.grantsFor(c, rbac.PrincipalFrom(c), sshProtocols)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, items)
}

// myDatabases 返回当前用户可连接的数据库资产账号（数据库控制台使用）。
func (m *Module) myDatabases(c *gin.Context) {
	items, err := m.grantsFor(c, rbac.PrincipalFrom(c), dbProtocols)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, items)
}

// authorized reports whether p may connect with account on asset via an active rule containing the connect action.
func (m *Module) authorized(ctx context.Context, p *rbac.Principal, assetID, accountID int64) (bool, error) {
	var n int
	err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM (`+grantSQL+`) g WHERE g.asset_id=$4 AND g.account_id=$5 AND 'connect' = ANY(g.actions)`,
		itoa(p.UserID), roles(p), []string{"ssh", "mysql", "postgres"}, assetID, accountID).Scan(&n)
	return n > 0, err
}

// ---------- command filters ----------

type filterIn struct {
	Name      string  `json:"name" binding:"required,max=120"`
	Pattern   string  `json:"pattern" binding:"required,max=512"`
	Action    string  `json:"action" binding:"required,oneof=deny confirm approve allow"`
	Priority  int     `json:"priority"`
	Enabled   *bool   `json:"enabled"`
	GroupName string  `json:"group_name" binding:"max=64"`
	AssetIDs  []int64 `json:"asset_ids"`
}

func (in *filterIn) check() error {
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return httpx.Validation("invalid regex: " + err.Error())
	}
	if re.MatchString("") {
		return httpx.Validation("pattern must not match an empty command")
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	if in.Priority < 1 || in.Priority > 100000 {
		return httpx.Validation("priority must be 1-100000")
	}
	in.GroupName = strings.TrimSpace(in.GroupName)
	if in.GroupName == "" {
		in.GroupName = "默认规则组"
	}
	if in.AssetIDs == nil {
		in.AssetIDs = []int64{}
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
	f, err := queryOne[Filter](c, m, `INSERT INTO bastion_filters (name,pattern,action,priority,enabled,group_name,asset_ids) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *`,
		in.Name, in.Pattern, in.Action, in.Priority, in.Enabled == nil || *in.Enabled, in.GroupName, in.AssetIDs)
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
	f, err := queryOne[Filter](c, m, `UPDATE bastion_filters SET name=$2,pattern=$3,action=$4,priority=$5,enabled=$6,group_name=$7,asset_ids=$8 WHERE id=$1 RETURNING *`,
		id, in.Name, in.Pattern, in.Action, in.Priority, in.Enabled == nil || *in.Enabled, in.GroupName, in.AssetIDs)
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

// loadFilters 读取对 assetID 生效的规则（作用资产为空表示全部资产），按优先级排序。
func (m *Module) loadFilters(ctx context.Context, assetID int64) ([]FilterRule, error) {
	fs, err := queryAll[Filter](ctx, m, `SELECT * FROM bastion_filters WHERE enabled AND (cardinality(asset_ids)=0 OR $1 = ANY(asset_ids)) ORDER BY priority, id`, assetID)
	if err != nil {
		return nil, err
	}
	out := make([]FilterRule, 0, len(fs))
	for _, f := range fs {
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			return nil, httpx.Conflict("an enabled command filter is invalid; connections are disabled")
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
		items[i].HasRecording = m.hasRecording(&items[i])
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
	s.HasRecording = m.hasRecording(s)
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
	f, err := m.openRecording(s)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("recording unavailable or not finalized"))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		httpx.Fail(c, httpx.Conflict("recording unavailable"))
		return
	}
	c.Header("Content-Type", "application/x-asciicast")
	c.Header("Content-Disposition", `attachment; filename="`+s.RecordingPath+`"`)
	http.ServeContent(c.Writer, c.Request, s.RecordingPath, st.ModTime(), f)
}

func (m *Module) killSession(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	audit.SetResource(c, "bastion_session", itoa(id))
	store := &pgLifecycleStore{db: m.d.DB}
	result, err := store.requestKill(c.Request.Context(), id, rbac.PrincipalFrom(c).UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.Conflict("session is not active with a live owner lease; kill was not requested"))
		return
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	// The request can land on any Pod. Only the owner may acknowledge after its
	// transports and recording have closed. Pending is deliberately HTTP 202.
	wait, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-wait.Done():
			c.JSON(http.StatusAccepted, httpx.Envelope{Code: httpx.CodeOK, Message: "kill requested; acknowledgement pending", Data: gin.H{"id": id, "requested": true, "killed": false, "request_id": result.RequestID, "status": "pending"}})
			return
		case <-tick.C:
			state, err := store.killState(wait, id, result.RequestID)
			if err != nil {
				if wait.Err() != nil {
					continue
				}
				httpx.Fail(c, err)
				return
			}
			if state.killed() {
				ok(c, gin.H{"id": id, "requested": true, "killed": true, "request_id": state.RequestID, "status": state.Status})
				return
			}
			if state.Status != "active" {
				c.JSON(http.StatusAccepted, httpx.Envelope{Code: httpx.CodeOK, Message: "kill request was not acknowledged", Data: gin.H{"id": id, "requested": true, "killed": false, "request_id": state.RequestID, "status": state.Status}})
				return
			}
		}
	}
}

func (m *Module) hasRecording(s *Session) bool {
	f, err := m.openRecording(s)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
