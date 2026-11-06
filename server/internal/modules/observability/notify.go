package observability

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/mail"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ChannelConfig holds per-type settings; stored encrypted as a whole.
type ChannelConfig struct {
	Webhook  string            `json:"webhook,omitempty"`  // robot / slack / generic webhook URL
	Secret   string            `json:"secret,omitempty"`   // dingtalk / feishu signing secret, webhook HMAC key
	Headers  map[string]string `json:"headers,omitempty"`  // generic webhook custom request headers (values are secret)
	Host     string            `json:"host,omitempty"`     // email SMTP
	Port     int               `json:"port,omitempty"`     // 465 = implicit TLS, else STARTTLS when offered
	Username string            `json:"username,omitempty"` //
	Password string            `json:"password,omitempty"` //
	From     string            `json:"from,omitempty"`     //
	To       []string          `json:"to,omitempty"`       //
	// UseSystem sends email through the platform system mail (settings mail.smtp);
	// only To is used from this channel then.
	UseSystem bool `json:"use_system,omitempty"`
}

// Channel is a notification channel.
type Channel struct {
	ID          int64            `json:"id"`
	Name        string           `json:"name"`
	Type        string           `json:"type"`
	Description string           `json:"description"`
	Config      ChannelConfig    `json:"config"` // secrets masked in responses
	Enabled     bool             `json:"enabled"`
	Severities  []string         `json:"severities"` // empty = all
	Envs        []string         `json:"envs"`       // empty = all
	Templates   map[string]int64 `json:"templates"`  // event kind -> template id; missing = built-in default
	LastSentAt  *time.Time       `json:"last_sent_at"`
	LastStatus  string           `json:"last_status"`
	CreatedBy   string           `json:"created_by"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return crypto.Mask(raw)
	}
	return u.Scheme + "://" + u.Host + "/" + crypto.Mask(strings.TrimPrefix(u.RequestURI(), "/"))
}

func (ch Channel) masked() Channel {
	c := ch
	if c.Config.Webhook != "" {
		c.Config.Webhook = maskURL(c.Config.Webhook)
	}
	if c.Config.Secret != "" {
		c.Config.Secret = crypto.Mask(c.Config.Secret)
	}
	if c.Config.Password != "" {
		c.Config.Password = crypto.Mask(c.Config.Password)
	}
	if len(c.Config.Headers) > 0 {
		h := make(map[string]string, len(c.Config.Headers))
		for k, v := range c.Config.Headers {
			h[k] = crypto.Mask(v)
		}
		c.Config.Headers = h
	}
	return c
}

// mergeSecrets keeps stored secret values when the caller sends them empty or
// masked, so editing never requires re-entering them.
func mergeSecrets(n *ChannelConfig, cur ChannelConfig) {
	mk := Channel{Config: cur}.masked().Config
	if n.Secret == "" || n.Secret == mk.Secret {
		n.Secret = cur.Secret
	}
	if n.Password == "" || n.Password == mk.Password {
		n.Password = cur.Password
	}
	if n.Webhook == "" || n.Webhook == mk.Webhook {
		n.Webhook = cur.Webhook
	}
	for k, v := range n.Headers {
		if old, ok := cur.Headers[k]; ok && (v == "" || v == mk.Headers[k]) {
			n.Headers[k] = old
		}
	}
}

type channelInput struct {
	Name        string           `json:"name" binding:"required,max=64"`
	Type        string           `json:"type" binding:"required"`
	Description string           `json:"description" binding:"max=256"`
	Config      *ChannelConfig   `json:"config"` // nil on update = keep
	Enabled     *bool            `json:"enabled"`
	Severities  []string         `json:"severities"`
	Envs        []string         `json:"envs"`
	Templates   map[string]int64 `json:"templates"` // nil on update = keep
	// SubscribeAlerts (create only) also creates an alert subscription that
	// routes every alert within the channel's severities / envs to it.
	SubscribeAlerts bool `json:"subscribe_alerts"`
}

var headerNameRe = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$")
var forbiddenHeaders = map[string]bool{"host": true, "content-length": true, "content-type": true, "transfer-encoding": true,
	"connection": true, "x-matrix-signature": true, "x-matrix-signature-v2": true, "x-matrix-timestamp": true}

// validateChannel checks the static shape of a configuration (no network).
func validateChannel(typ string, cfg *ChannelConfig) error {
	switch typ {
	case "dingtalk", "feishu", "wecom", "slack", "webhook":
		u, err := url.Parse(strings.TrimSpace(cfg.Webhook))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return httpx.Validation("Webhook 必须是不含账号密码的 http(s) 地址")
		}
		cfg.Webhook = strings.TrimSpace(cfg.Webhook)
		if typ != "webhook" {
			cfg.Headers = nil
		}
		if len(cfg.Headers) > 20 {
			return httpx.Validation("自定义请求头最多 20 个")
		}
		for k, v := range cfg.Headers {
			if !headerNameRe.MatchString(k) || forbiddenHeaders[strings.ToLower(k)] {
				return httpx.Validation("请求头名称不合法或为保留头：" + k)
			}
			if strings.ContainsAny(v, "\r\n") || len(v) > 4096 {
				return httpx.Validation("请求头 " + k + " 的值不合法")
			}
		}
		cfg.Host, cfg.Port, cfg.Username, cfg.Password, cfg.From, cfg.To, cfg.UseSystem = "", 0, "", "", "", nil, false
	case "email":
		cfg.Webhook, cfg.Secret, cfg.Headers = "", "", nil
		if len(cfg.To) == 0 {
			return httpx.Validation("请填写收件人")
		}
		if len(cfg.To) > 50 {
			return httpx.Validation("收件人最多 50 个")
		}
		addrs := append([]string{}, cfg.To...)
		if !cfg.UseSystem {
			cfg.Host = strings.TrimSpace(cfg.Host)
			if cfg.Host == "" || cfg.From == "" {
				return httpx.Validation("自定义 SMTP 需要填写服务器与发件人")
			}
			if strings.ContainsAny(cfg.Host, "/: \t") && net.ParseIP(cfg.Host) == nil {
				return httpx.Validation("SMTP 服务器只填主机名或 IP，不含协议和端口")
			}
			if cfg.Port == 0 {
				cfg.Port = 25
			}
			if cfg.Port < 1 || cfg.Port > 65535 {
				return httpx.Validation("SMTP 端口必须在 1~65535")
			}
			addrs = append(addrs, cfg.From)
		} else {
			cfg.Host, cfg.Port, cfg.Username, cfg.Password, cfg.From = "", 0, "", "", ""
		}
		for _, a := range addrs {
			if strings.ContainsAny(a, "\r\n<>,; ") || !strings.Contains(a, "@") {
				return httpx.Validation("邮箱地址不合法：" + strconv.Quote(a))
			}
		}
	default:
		return httpx.Validation("类型必须是 dingtalk|feishu|wecom|slack|webhook|email")
	}
	return nil
}

// checkChannelTarget applies the outbound address policy at save / test time.
func (m *Module) checkChannelTarget(ctx context.Context, typ string, cfg *ChannelConfig, pol netPolicy) error {
	switch typ {
	case "email":
		if cfg.UseSystem {
			return nil // platform system mail is administrator-configured
		}
		return pol.checkHost(ctx, cfg.Host, strconv.Itoa(cfg.Port))
	case "webhook":
		return pol.checkURL(ctx, cfg.Webhook, false)
	default:
		return pol.checkURL(ctx, cfg.Webhook, true)
	}
}

func (m *Module) validateTemplates(ctx context.Context, tm map[string]int64) error {
	for kind, id := range tm {
		if !validKind(kind) {
			return httpx.Validation("模板映射的事件类型不正确：" + kind)
		}
		if id <= 0 {
			delete(tm, kind)
			continue
		}
		t, err := m.loadTemplate(ctx, id)
		if err != nil {
			return httpx.Validation("所选模板不存在")
		}
		if t.EventKind != kind {
			return httpx.Validation("模板「" + t.Name + "」用于" + or(kindText, t.EventKind) + "，不能用于" + or(kindText, kind))
		}
	}
	return nil
}

const chCols = `id,name,type,description,config_enc,enabled,severities,envs,templates,last_sent_at,last_status,created_by,created_at,updated_at`

func (m *Module) scanChannel(row pgx.Row) (*Channel, error) {
	var ch Channel
	var enc string
	var tm []byte
	if err := row.Scan(&ch.ID, &ch.Name, &ch.Type, &ch.Description, &enc, &ch.Enabled, &ch.Severities, &ch.Envs, &tm,
		&ch.LastSentAt, &ch.LastStatus, &ch.CreatedBy, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("渠道不存在")
		}
		return nil, err
	}
	plain, err := m.d.Crypto.Decrypt(enc)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := json.Unmarshal([]byte(plain), &ch.Config); err != nil {
		return nil, httpx.Internal(err)
	}
	_ = json.Unmarshal(tm, &ch.Templates)
	if ch.Templates == nil {
		ch.Templates = map[string]int64{}
	}
	return &ch, nil
}

func (m *Module) channelByID(ctx context.Context, id int64) (*Channel, error) {
	return m.scanChannel(m.d.DB.QueryRow(ctx, `SELECT `+chCols+` FROM obs_channels WHERE id=$1`, id))
}

func (m *Module) loadChannel(c *gin.Context) (*Channel, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, false
	}
	ch, err := m.channelByID(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	return ch, true
}

// channelView is a masked channel plus the subscriptions referencing it.
type channelView struct {
	Channel
	Subscriptions []subRef `json:"subscriptions"`
}

type subRef struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func (m *Module) channelSubs(ctx context.Context) (map[int64][]subRef, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT id,name,enabled,channel_ids FROM obs_notify_subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]subRef{}
	for rows.Next() {
		var r subRef
		var ids []int64
		if err := rows.Scan(&r.ID, &r.Name, &r.Enabled, &ids); err != nil {
			return nil, err
		}
		for _, id := range ids {
			out[id] = append(out[id], r)
		}
	}
	return out, rows.Err()
}

func (m *Module) view(ch *Channel, refs map[int64][]subRef) channelView {
	v := channelView{Channel: ch.masked(), Subscriptions: refs[ch.ID]}
	if v.Subscriptions == nil {
		v.Subscriptions = []subRef{}
	}
	return v
}

func (m *Module) listChannels(c *gin.Context) {
	rows, err := m.d.DB.Query(c, `SELECT `+chCols+` FROM obs_channels ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var chans []*Channel
	for rows.Next() {
		ch, err := m.scanChannel(rows)
		if err != nil {
			rows.Close()
			httpx.Fail(c, err)
			return
		}
		chans = append(chans, ch)
	}
	rows.Close()
	refs, err := m.channelSubs(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := []channelView{}
	for _, ch := range chans {
		out = append(out, m.view(ch, refs))
	}
	httpx.OK(c, out)
}

func (m *Module) getChannel(c *gin.Context) {
	ch, ok := m.loadChannel(c)
	if !ok {
		return
	}
	refs, err := m.channelSubs(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, m.view(ch, refs))
}

func (m *Module) encConfig(cfg *ChannelConfig) (string, error) {
	b, _ := json.Marshal(cfg)
	return m.d.Crypto.Encrypt(string(b))
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (m *Module) createChannel(c *gin.Context) {
	var in channelInput
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Config == nil {
		httpx.Fail(c, httpx.Validation("缺少渠道配置"))
		return
	}
	if err := validateChannel(in.Type, in.Config); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.checkChannelTarget(c, in.Type, in.Config, m.netPolicy(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.Templates == nil {
		in.Templates = map[string]int64{}
	}
	if err := m.validateTemplates(c, in.Templates); err != nil {
		httpx.Fail(c, err)
		return
	}
	enc, err := m.encConfig(in.Config)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	tm, _ := json.Marshal(in.Templates)
	enabled := in.Enabled == nil || *in.Enabled
	who := rbac.PrincipalFrom(c).Username
	tx, err := m.d.DB.Begin(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer tx.Rollback(c) //nolint:errcheck
	var id int64
	err = tx.QueryRow(c, `INSERT INTO obs_channels (name,type,description,config_enc,enabled,severities,envs,templates,created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		strings.TrimSpace(in.Name), in.Type, in.Description, enc, enabled, nonNil(in.Severities), nonNil(in.Envs), tm, who).Scan(&id)
	if err != nil {
		httpx.Fail(c, dupErr(err, "渠道名称已存在"))
		return
	}
	if in.SubscribeAlerts {
		if _, err := tx.Exec(c, `INSERT INTO obs_notify_subscriptions (name,description,event_kinds,severities,envs,channel_ids,created_by)
			VALUES ($1,'创建渠道时自动生成','{alert}',$2,$3,$4,$5)`, "告警 → "+strings.TrimSpace(in.Name), nonNil(in.Severities), nonNil(in.Envs), []int64{id}, who); err != nil {
			httpx.Fail(c, dupErr(err, "同名告警订阅已存在，请取消「自动订阅告警」或更换渠道名称"))
			return
		}
	}
	if err := tx.Commit(c); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_channel", strconv.FormatInt(id, 10))
	ch, err := m.channelByID(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	refs, _ := m.channelSubs(c)
	httpx.Created(c, m.view(ch, refs))
}

func (m *Module) updateChannel(c *gin.Context) {
	cur, ok := m.loadChannel(c)
	if !ok {
		return
	}
	var in channelInput
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Type != cur.Type {
		httpx.Fail(c, httpx.Validation("渠道类型不能修改"))
		return
	}
	cfg := cur.Config
	changedTarget := false
	if in.Config != nil {
		n := *in.Config
		mergeSecrets(&n, cur.Config)
		changedTarget = n.Webhook != cur.Config.Webhook || n.Host != cur.Config.Host || n.Port != cur.Config.Port || n.UseSystem != cur.Config.UseSystem
		cfg = n
	}
	if err := validateChannel(cur.Type, &cfg); err != nil {
		httpx.Fail(c, err)
		return
	}
	if changedTarget {
		if err := m.checkChannelTarget(c, cur.Type, &cfg, m.netPolicy(c)); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	tmpl := cur.Templates
	if in.Templates != nil {
		tmpl = in.Templates
		if err := m.validateTemplates(c, tmpl); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	enc, err := m.encConfig(&cfg)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	desc := in.Description
	if in.Config == nil && desc == "" {
		desc = cur.Description // a bare enable/disable PUT keeps the description
	}
	tm, _ := json.Marshal(tmpl)
	_, err = m.d.DB.Exec(c, `UPDATE obs_channels SET name=$2,description=$3,config_enc=$4,enabled=$5,severities=$6,envs=$7,templates=$8,updated_at=now() WHERE id=$1`,
		cur.ID, strings.TrimSpace(in.Name), desc, enc, enabled, nonNil(in.Severities), nonNil(in.Envs), tm)
	if err != nil {
		httpx.Fail(c, dupErr(err, "渠道名称已存在"))
		return
	}
	audit.SetResource(c, "obs_channel", strconv.FormatInt(cur.ID, 10))
	ch, err := m.channelByID(c, cur.ID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	refs, _ := m.channelSubs(c)
	httpx.OK(c, m.view(ch, refs))
}

// setChannelEnabled toggles a channel without touching its configuration.
func (m *Module) setChannelEnabled(c *gin.Context) {
	ch, ok := m.loadChannel(c)
	if !ok {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	_, err := m.d.DB.Exec(c, `UPDATE obs_channels SET enabled=$2,updated_at=now() WHERE id=$1`, ch.ID, *in.Enabled)
	audit.SetResource(c, "obs_channel", strconv.FormatInt(ch.ID, 10))
	audit.SetDetail(c, map[string]any{"enabled": *in.Enabled})
	respond(c, gin.H{"id": ch.ID, "enabled": *in.Enabled}, err)
}

func (m *Module) channelReferences(c *gin.Context) {
	ch, ok := m.loadChannel(c)
	if !ok {
		return
	}
	refs, err := m.channelSubs(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := refs[ch.ID]
	if out == nil {
		out = []subRef{}
	}
	httpx.OK(c, gin.H{"subscriptions": out})
}

// deleteChannel refuses while subscriptions reference the channel unless
// ?force=true; forced deletion removes it from those subscriptions and
// disables any subscription left without channels.
func (m *Module) deleteChannel(c *gin.Context) {
	ch, ok := m.loadChannel(c)
	if !ok {
		return
	}
	refs, err := m.channelSubs(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	used := refs[ch.ID]
	if len(used) > 0 && c.Query("force") != "true" {
		names := make([]string, 0, len(used))
		for _, r := range used {
			names = append(names, r.Name)
		}
		httpx.Fail(c, httpx.Conflict("渠道正被订阅引用："+strings.Join(names, "、")).WithDetails(gin.H{"subscriptions": used}))
		return
	}
	tx, err := m.d.DB.Begin(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer tx.Rollback(c) //nolint:errcheck
	if _, err := tx.Exec(c, `UPDATE obs_notify_subscriptions SET channel_ids=array_remove(channel_ids,$1),
		enabled = enabled AND cardinality(array_remove(channel_ids,$1)) > 0, updated_at=now() WHERE $1 = ANY(channel_ids)`, ch.ID); err != nil {
		httpx.Fail(c, err)
		return
	}
	if _, err := tx.Exec(c, `DELETE FROM obs_channels WHERE id=$1`, ch.ID); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := tx.Commit(c); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_channel", strconv.FormatInt(ch.ID, 10))
	httpx.OK(c, gin.H{"deleted": ch.ID, "subscriptions_updated": len(used)})
}

// testChannelDraft sends a test message with an unsaved configuration (used
// by the create / edit form). When id is given, empty secret fields fall back
// to the stored channel's values, so editing never requires re-entering them.
func (m *Module) testChannelDraft(c *gin.Context) {
	var in struct {
		channelInput
		ID int64 `json:"id"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Config == nil {
		httpx.Fail(c, httpx.Validation("缺少渠道配置"))
		return
	}
	cfg := *in.Config
	if in.ID != 0 {
		cur, err := m.channelByID(c, in.ID)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if cur.Type != in.Type {
			httpx.Fail(c, httpx.Validation("渠道类型不能修改"))
			return
		}
		mergeSecrets(&cfg, cur.Config)
	}
	if err := validateChannel(in.Type, &cfg); err != nil {
		httpx.Fail(c, err)
		return
	}
	pol := m.netPolicy(c)
	if err := m.checkChannelTarget(c, in.Type, &cfg, pol); err != nil {
		httpx.Fail(c, err)
		return
	}
	ch := &Channel{ID: in.ID, Name: in.Name, Type: in.Type, Config: cfg, Templates: in.Templates}
	httpx.OK(c, m.sendTest(c, ch, rbac.PrincipalFrom(c).Username))
}

func (m *Module) testChannel(c *gin.Context) {
	ch, ok := m.loadChannel(c)
	if !ok {
		return
	}
	httpx.OK(c, m.sendTest(c, ch, rbac.PrincipalFrom(c).Username))
}

// sendTest renders the channel's test template, sends it and records the attempt.
func (m *Module) sendTest(ctx context.Context, ch *Channel, who string) gin.H {
	ev := NotifyEvent{Kind: "test", Status: "test", Severity: "info", Key: "test:" + strconv.FormatInt(ch.ID, 10), Actor: who,
		StartsAtT: time.Now(), Labels: map[string]string{"channel": ch.Name, "channel_type": or(channelTypeText, ch.Type)}}
	title, body, err := m.renderFor(ctx, ch, &ev)
	if err != nil {
		return gin.H{"ok": false, "error": err.Error()}
	}
	start := time.Now()
	err = m.sender(ctx).send(ctx, ch, Message{Title: title, Body: body, Event: &ev})
	rec := &notifyRecord{ChannelID: ch.ID, ChannelName: ch.Name, ChannelType: ch.Type, EventKind: "test", EventStatus: "test",
		EventKey: ev.Key, Title: title, Body: body, TriggeredBy: who, DurationMS: int(time.Since(start).Milliseconds())}
	rec.setResult(err)
	rec.event = &ev
	if ch.ID != 0 { // a draft that was never saved has nothing to resend to
		m.saveRecord(ctx, rec)
	}
	res := gin.H{"ok": err == nil, "duration_ms": rec.DurationMS}
	if err != nil {
		res["error"] = err.Error()
	}
	return res
}

var channelTypeText = map[string]string{"dingtalk": "钉钉", "feishu": "飞书", "wecom": "企业微信", "email": "邮件", "slack": "Slack", "webhook": "通用 Webhook"}

// ------------------------------------------------------------------- sending

// Message is one rendered notification.
type Message struct {
	Title string
	Body  string
	Event *NotifyEvent // optional; included in generic webhook payloads
}

// sender delivers messages under an outbound address policy.
type sender struct {
	policy     netPolicy
	client     *http.Client
	systemMail func(ctx context.Context) (mail.Config, error)
}

func newSender(p netPolicy) *sender {
	return &sender{policy: p, client: p.httpClient(15 * time.Second)}
}

func (m *Module) sender(ctx context.Context) *sender {
	s := newSender(m.netPolicy(ctx))
	s.systemMail = func(ctx context.Context) (mail.Config, error) {
		var cfg mail.Config
		if m.d == nil || m.d.Settings == nil {
			return cfg, errors.New("系统邮件未配置")
		}
		if err := m.d.Settings.Get(ctx, mail.SettingKey, &cfg); err != nil {
			return cfg, errors.New("系统邮件未配置")
		}
		return cfg, nil
	}
	return s
}

// sendChannel delivers one message through ch without an address policy.
// Module code uses m.sender(ctx).send, which enforces the stored policy.
func sendChannel(ctx context.Context, ch *Channel, title, body string) error {
	return newSender(permissivePolicy()).send(ctx, ch, Message{Title: title, Body: body})
}

func hmacB64(key, msg string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func hmacHex(key string, msg []byte) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write(msg)
	return hex.EncodeToString(h.Sum(nil))
}

// truncRunes cuts s to n runes (robots reject oversized messages).
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "\n…（内容过长已截断）"
}

// truncBytes cuts s to at most n bytes on a rune boundary.
func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const tail = "\n…（已截断）"
	cut := n - len(tail)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + tail
}

// buildPayload returns the JSON body a robot / webhook channel receives
// (without signatures). It is also used by the template preview.
func buildPayload(ch *Channel, msg Message, now time.Time) map[string]any {
	title, body := msg.Title, msg.Body
	switch ch.Type {
	case "dingtalk":
		return map[string]any{"msgtype": "markdown",
			"markdown": map[string]any{"title": title, "text": "### " + title + "\n\n" + mdLines(truncRunes(body, 6000))}}
	case "feishu":
		return map[string]any{"msg_type": "text", "content": map[string]any{"text": title + "\n" + truncRunes(body, 6000)}}
	case "wecom": // markdown content is limited to 4096 bytes
		return map[string]any{"msgtype": "markdown",
			"markdown": map[string]any{"content": truncBytes("**"+title+"**\n"+body, 4000)}}
	case "slack":
		return map[string]any{"text": "*" + title + "*\n" + truncRunes(body, 3500)}
	case "webhook":
		p := map[string]any{"source": "xingshu-matrix", "channel": ch.Name, "title": title, "text": body,
			"sent_at": now.UTC().Format(time.RFC3339)}
		if msg.Event != nil {
			e := msg.Event
			ev := map[string]any{"kind": e.Kind, "status": e.Status, "key": e.Key, "alert_name": e.AlertName, "severity": e.Severity,
				"env": e.Env, "cluster": e.Cluster, "namespace": e.Namespace, "resource": e.Resource, "summary": e.Summary,
				"link": e.Link, "labels": e.Labels}
			if !e.StartsAtT.IsZero() {
				ev["starts_at"] = e.StartsAtT.UTC().Format(time.RFC3339)
			}
			if e.ID != 0 {
				ev["id"] = e.ID
			}
			p["event"] = ev
		}
		return p
	case "email":
		return map[string]any{"subject": title, "text": body}
	}
	return nil
}

// webhookSignature returns the generic webhook signature headers:
//
//	X-Matrix-Signature:    sha256=hex(HMAC-SHA256(secret, body))            (v1, unchanged)
//	X-Matrix-Timestamp:    unix seconds
//	X-Matrix-Signature-V2: sha256=hex(HMAC-SHA256(secret, timestamp+"."+body)) (replay-resistant)
func webhookSignature(secret string, body []byte, ts int64) map[string]string {
	t := strconv.FormatInt(ts, 10)
	return map[string]string{
		"X-Matrix-Signature":    "sha256=" + hmacHex(secret, body),
		"X-Matrix-Timestamp":    t,
		"X-Matrix-Signature-V2": "sha256=" + hmacHex(secret, append([]byte(t+"."), body...)),
	}
}

// dingtalkSignedURL appends DingTalk's timestamp / sign query parameters.
func dingtalkSignedURL(u, secret string, now time.Time) string {
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	sign := hmacB64(secret, ts+"\n"+secret)
	return u + sep(u) + "timestamp=" + ts + "&sign=" + url.QueryEscape(sign)
}

// feishuSign returns Feishu's signature: HMAC-SHA256 keyed by "timestamp\nsecret" over an empty message.
func feishuSign(secret string, ts int64) string {
	return hmacB64(strconv.FormatInt(ts, 10)+"\n"+secret, "")
}

func (s *sender) send(ctx context.Context, ch *Channel, msg Message) error {
	cfg := ch.Config
	now := time.Now()
	payload := buildPayload(ch, msg, now)
	switch ch.Type {
	case "dingtalk":
		u := cfg.Webhook
		if cfg.Secret != "" {
			u = dingtalkSignedURL(u, cfg.Secret, now)
		}
		return s.postJSON(ctx, u, payload, nil)
	case "feishu":
		if cfg.Secret != "" {
			payload["timestamp"] = strconv.FormatInt(now.Unix(), 10)
			payload["sign"] = feishuSign(cfg.Secret, now.Unix())
		}
		return s.postJSON(ctx, cfg.Webhook, payload, nil)
	case "wecom", "slack":
		return s.postJSON(ctx, cfg.Webhook, payload, nil)
	case "webhook":
		b, _ := json.Marshal(payload)
		hdr := map[string]string{}
		for k, v := range cfg.Headers {
			hdr[k] = v
		}
		if cfg.Secret != "" {
			for k, v := range webhookSignature(cfg.Secret, b, now.Unix()) {
				hdr[k] = v
			}
		}
		return s.postRaw(ctx, cfg.Webhook, b, hdr)
	case "email":
		if cfg.UseSystem {
			if s.systemMail == nil {
				return errors.New("系统邮件未配置")
			}
			sys, err := s.systemMail(ctx)
			if err != nil {
				return err
			}
			return mail.Send(ctx, sys, cfg.To, msg.Title, msg.Body)
		}
		return s.sendMail(ctx, cfg, msg.Title, msg.Body)
	}
	return fmt.Errorf("不支持的渠道类型 %s", ch.Type)
}

func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

func mdLines(s string) string { return strings.ReplaceAll(s, "\n", "  \n") }

func (s *sender) postJSON(ctx context.Context, u string, payload any, hdr map[string]string) error {
	b, _ := json.Marshal(payload)
	return s.postRaw(ctx, u, b, hdr)
}

// postRaw posts JSON and checks the robot errcode/code field.
func (s *sender) postRaw(ctx context.Context, u string, b []byte, hdr map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "xingshu-matrix-notify/1.0")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.client.Do(req)
	if err != nil {
		return scrubURLError(err, u)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 3 {
		return fmt.Errorf("HTTP %d：对方返回重定向，出于安全考虑不跟随", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstN(string(rb), 300))
	}
	var r struct {
		ErrCode *int   `json:"errcode"` // dingtalk / wecom
		ErrMsg  string `json:"errmsg"`
		Code    *int   `json:"code"` // feishu
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(rb, &r) == nil {
		if r.ErrCode != nil && *r.ErrCode != 0 {
			return fmt.Errorf("robot error %d: %s", *r.ErrCode, r.ErrMsg)
		}
		if r.Code != nil && *r.Code != 0 {
			return fmt.Errorf("robot error %d: %s", *r.Code, r.Msg)
		}
	}
	return nil
}

// scrubURLError removes the request URL (it contains robot tokens) from errors.
func scrubURLError(err error, u string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if u != "" {
		msg = strings.ReplaceAll(msg, u, maskURL(u))
	}
	return errors.New(msg)
}

func (s *sender) sendMail(ctx context.Context, cfg ChannelConfig, subject, body string) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := &net.Dialer{Timeout: 15 * time.Second}
	dial := s.policy.dialContext(d)
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if cfg.Port == 465 {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return err
		}
		conn = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if cfg.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return err
			}
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return err
	}
	for _, to := range cfg.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(buildMail(cfg.From, cfg.To, subject, body, time.Now()))); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// buildMail renders an RFC 5322 message with header-injection stripping.
func buildMail(from string, to []string, subject, body string, now time.Time) string {
	clean := func(s string) string { return strings.NewReplacer("\r", "", "\n", "").Replace(s) }
	return "From: " + clean(from) + "\r\nTo: " + clean(strings.Join(to, ", ")) +
		"\r\nSubject: " + mime.BEncoding.Encode("UTF-8", clean(subject)) +
		"\r\nDate: " + now.Format(time.RFC1123Z) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		wrap76(base64.StdEncoding.EncodeToString([]byte(body)))
}

func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76] + "\r\n")
		s = s[76:]
	}
	b.WriteString(s + "\r\n")
	return b.String()
}

// formatAlert keeps the legacy plain rendering (used by tests and as a fallback).
func formatAlert(a Alert) (string, string) {
	ev := alertEvent(a, "")
	b := builtinTemplates["alert"]
	title, body, err := renderTemplate(b.TitleTpl, b.BodyTpl, &ev)
	if err != nil {
		return a.Alertname, a.Alertname
	}
	return title, body
}

// alertEvent normalizes a stored alert.
func alertEvent(a Alert, link string) NotifyEvent {
	ev := NotifyEvent{Kind: "alert", Status: a.Status, ID: a.ID, AlertName: a.Alertname,
		Severity: normSeverity(a.Severity), SeverityRaw: a.Severity, Env: a.Env, Link: link, Labels: map[string]string{}}
	if ev.Status == "" {
		ev.Status = "firing"
	}
	if ev.Severity == "" {
		ev.Severity = "warning"
	}
	for k, v := range a.Labels {
		ev.Labels[k] = v
	}
	ev.Cluster = firstNonEmpty(a.Labels["cluster"], a.Labels["cluster_name"], a.Labels["k8s_cluster"], a.Labels["kubernetes_cluster"])
	ev.Namespace = firstNonEmpty(a.Labels["namespace"], a.Labels["exported_namespace"])
	for _, k := range []string{"pod", "deployment", "statefulset", "daemonset", "job", "node", "service", "persistentvolumeclaim", "instance"} {
		if v := a.Labels[k]; v != "" {
			ev.Resource = k + "/" + v
			break
		}
	}
	ev.Summary = firstNonEmpty(a.Annotations["summary"], a.Annotations["message"])
	ev.Description = a.Annotations["description"]
	if a.StartsAt != nil {
		ev.StartsAtT = *a.StartsAt
	}
	fp := a.Fingerprint
	if fp == "" {
		fp = strconv.FormatInt(a.ID, 10)
	}
	ev.Key = "alert:" + strconv.FormatInt(a.DatasourceID, 10) + ":" + fp + ":" + ev.Status
	if link == "" && a.GeneratorURL != "" {
		ev.Link = a.GeneratorURL
	}
	return ev
}
