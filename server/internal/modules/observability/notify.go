package observability

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ChannelConfig holds per-type settings; stored encrypted as a whole.
type ChannelConfig struct {
	Webhook  string   `json:"webhook,omitempty"`  // dingtalk / feishu / wecom robot URL
	Secret   string   `json:"secret,omitempty"`   // dingtalk / feishu signing secret (optional)
	Host     string   `json:"host,omitempty"`     // email SMTP
	Port     int      `json:"port,omitempty"`     // 465 = implicit TLS, else STARTTLS when offered
	Username string   `json:"username,omitempty"` //
	Password string   `json:"password,omitempty"` //
	From     string   `json:"from,omitempty"`     //
	To       []string `json:"to,omitempty"`       //
}

// Channel is a notification channel.
type Channel struct {
	ID         int64         `json:"id"`
	Name       string        `json:"name"`
	Type       string        `json:"type"`
	Config     ChannelConfig `json:"config"` // secrets masked in responses
	Enabled    bool          `json:"enabled"`
	Severities []string      `json:"severities"` // empty = all
	Envs       []string      `json:"envs"`       // empty = all
	CreatedBy  string        `json:"created_by"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

func (ch Channel) masked() Channel {
	c := ch
	if c.Config.Webhook != "" {
		if u, err := url.Parse(c.Config.Webhook); err == nil {
			c.Config.Webhook = u.Scheme + "://" + u.Host + "/" + crypto.Mask(u.RequestURI())
		}
	}
	if c.Config.Secret != "" {
		c.Config.Secret = crypto.Mask(c.Config.Secret)
	}
	if c.Config.Password != "" {
		c.Config.Password = crypto.Mask(c.Config.Password)
	}
	return c
}

type channelInput struct {
	Name       string         `json:"name" binding:"required,max=64"`
	Type       string         `json:"type" binding:"required"`
	Config     *ChannelConfig `json:"config"` // nil on update = keep
	Enabled    *bool          `json:"enabled"`
	Severities []string       `json:"severities"`
	Envs       []string       `json:"envs"`
}

func validateChannel(typ string, cfg *ChannelConfig) error {
	switch typ {
	case "dingtalk", "feishu", "wecom":
		u, err := url.Parse(cfg.Webhook)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return httpx.Validation("webhook must be an https URL")
		}
	case "email":
		if cfg.Host == "" || cfg.From == "" || len(cfg.To) == 0 {
			return httpx.Validation("email requires host, from and to")
		}
		if cfg.Port == 0 {
			cfg.Port = 25
		}
		for _, a := range append([]string{cfg.From}, cfg.To...) {
			if strings.ContainsAny(a, "\r\n") || !strings.Contains(a, "@") {
				return httpx.Validation("invalid email address " + strconv.Quote(a))
			}
		}
	default:
		return httpx.Validation("type must be dingtalk|feishu|wecom|email")
	}
	return nil
}

const chCols = `id,name,type,config_enc,enabled,severities,envs,created_by,created_at,updated_at`

func (m *Module) scanChannel(row pgx.Row) (*Channel, error) {
	var ch Channel
	var enc string
	if err := row.Scan(&ch.ID, &ch.Name, &ch.Type, &enc, &ch.Enabled, &ch.Severities, &ch.Envs, &ch.CreatedBy, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("channel not found")
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
	return &ch, nil
}

func (m *Module) loadChannel(c *gin.Context) (*Channel, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, false
	}
	ch, err := m.scanChannel(m.d.DB.QueryRow(c, `SELECT `+chCols+` FROM obs_channels WHERE id=$1`, id))
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	return ch, true
}

func (m *Module) listChannels(c *gin.Context) {
	rows, err := m.d.DB.Query(c, `SELECT `+chCols+` FROM obs_channels ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		ch, err := m.scanChannel(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		out = append(out, ch.masked())
	}
	httpx.OK(c, out)
}

func (m *Module) getChannel(c *gin.Context) {
	if ch, ok := m.loadChannel(c); ok {
		httpx.OK(c, ch.masked())
	}
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
		httpx.Fail(c, httpx.Validation("config required"))
		return
	}
	if err := validateChannel(in.Type, in.Config); err != nil {
		httpx.Fail(c, err)
		return
	}
	enc, err := m.encConfig(in.Config)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	enabled := in.Enabled == nil || *in.Enabled
	var id int64
	err = m.d.DB.QueryRow(c, `INSERT INTO obs_channels (name,type,config_enc,enabled,severities,envs,created_by) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		in.Name, in.Type, enc, enabled, nonNil(in.Severities), nonNil(in.Envs), rbac.PrincipalFrom(c).Username).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			err = httpx.Conflict("channel name already exists")
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_channel", strconv.FormatInt(id, 10))
	ch, err := m.scanChannel(m.d.DB.QueryRow(c, `SELECT `+chCols+` FROM obs_channels WHERE id=$1`, id))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.Created(c, ch.masked())
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
		httpx.Fail(c, httpx.Validation("type cannot be changed"))
		return
	}
	cfg := cur.Config
	if in.Config != nil {
		// empty secret fields keep the stored value
		n := *in.Config
		mk := cur.masked().Config
		if n.Secret == "" || n.Secret == mk.Secret {
			n.Secret = cfg.Secret
		}
		if n.Password == "" || n.Password == mk.Password {
			n.Password = cfg.Password
		}
		if n.Webhook == "" || n.Webhook == mk.Webhook {
			n.Webhook = cfg.Webhook
		}
		cfg = n
	}
	if err := validateChannel(cur.Type, &cfg); err != nil {
		httpx.Fail(c, err)
		return
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
	_, err = m.d.DB.Exec(c, `UPDATE obs_channels SET name=$2,config_enc=$3,enabled=$4,severities=$5,envs=$6,updated_at=now() WHERE id=$1`,
		cur.ID, in.Name, enc, enabled, nonNil(in.Severities), nonNil(in.Envs))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_channel", strconv.FormatInt(cur.ID, 10))
	ch, err := m.scanChannel(m.d.DB.QueryRow(c, `SELECT `+chCols+` FROM obs_channels WHERE id=$1`, cur.ID))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, ch.masked())
}

func (m *Module) deleteChannel(c *gin.Context) {
	ch, ok := m.loadChannel(c)
	if !ok {
		return
	}
	_, err := m.d.DB.Exec(c, `DELETE FROM obs_channels WHERE id=$1`, ch.ID)
	audit.SetResource(c, "obs_channel", strconv.FormatInt(ch.ID, 10))
	respond(c, gin.H{"deleted": ch.ID}, err)
}

func (m *Module) testChannel(c *gin.Context) {
	ch, ok := m.loadChannel(c)
	if !ok {
		return
	}
	title := "【星枢 Matrix】通知渠道测试"
	body := fmt.Sprintf("这是一条来自星枢 Matrix 的测试消息。\n渠道：%s (%s)\n操作人：%s\n时间：%s",
		ch.Name, ch.Type, rbac.PrincipalFrom(c).Username, time.Now().Format("2006-01-02 15:04:05 MST"))
	err := sendChannel(c, ch, title, body)
	res := gin.H{"ok": err == nil}
	if err != nil {
		res["error"] = err.Error()
	}
	httpx.OK(c, res)
}

// ------------------------------------------------------------------- dispatch

func contains(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// notifyAlerts sends newly firing alerts to every matching enabled channel.
func (m *Module) notifyAlerts(ctx context.Context, alerts []Alert) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rows, err := m.d.DB.Query(ctx, `SELECT `+chCols+` FROM obs_channels WHERE enabled`)
	if err != nil {
		m.logWarn("load channels", err)
		return
	}
	var chans []*Channel
	for rows.Next() {
		ch, err := m.scanChannel(rows)
		if err != nil {
			m.logWarn("decode channel", err)
			continue
		}
		chans = append(chans, ch)
	}
	rows.Close()
	for _, ch := range chans {
		for _, a := range alerts {
			if !contains(ch.Severities, a.Severity) || !contains(ch.Envs, a.Env) {
				continue
			}
			title, body := formatAlert(a)
			if err := sendChannel(ctx, ch, title, body); err != nil {
				m.logWarn("send notification", fmt.Errorf("channel %s alert %d: %w", ch.Name, a.ID, err))
			}
		}
	}
}

func (m *Module) logWarn(msg string, err error) {
	if m.d != nil && m.d.Logger != nil {
		m.d.Logger.Warn(msg, "err", err)
	}
}

func formatAlert(a Alert) (string, string) {
	sev := a.Severity
	if sev == "" {
		sev = "unknown"
	}
	title := fmt.Sprintf("【告警触发】[%s] %s", sev, a.Alertname)
	var b strings.Builder
	fmt.Fprintf(&b, "告警：%s\n级别：%s\n环境：%s\n数据源：%s\n", a.Alertname, sev, a.Env, a.Datasource)
	if a.StartsAt != nil {
		fmt.Fprintf(&b, "开始时间：%s\n", a.StartsAt.Local().Format("2006-01-02 15:04:05"))
	}
	for _, k := range []string{"summary", "description", "message"} {
		if v := a.Annotations[k]; v != "" {
			fmt.Fprintf(&b, "%s：%s\n", k, v)
		}
	}
	keys := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		if k != "alertname" && k != "severity" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		b.WriteString("标签：")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k + "=" + a.Labels[k])
		}
		b.WriteString("\n")
	}
	if a.GeneratorURL != "" {
		b.WriteString("来源：" + a.GeneratorURL + "\n")
	}
	return title, strings.TrimSpace(b.String())
}

var notifyHTTP = &http.Client{Timeout: 15 * time.Second}

func hmacB64(key, msg string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// sendChannel delivers one message through ch.
func sendChannel(ctx context.Context, ch *Channel, title, body string) error {
	cfg := ch.Config
	switch ch.Type {
	case "dingtalk":
		u := cfg.Webhook
		if cfg.Secret != "" {
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
			sign := hmacB64(cfg.Secret, ts+"\n"+cfg.Secret)
			u += sep(u) + "timestamp=" + ts + "&sign=" + url.QueryEscape(sign)
		}
		return postRobot(ctx, u, map[string]any{"msgtype": "markdown",
			"markdown": map[string]any{"title": title, "text": "### " + title + "\n\n" + mdLines(body)}})
	case "feishu":
		msg := map[string]any{"msg_type": "text", "content": map[string]any{"text": title + "\n" + body}}
		if cfg.Secret != "" {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			// Feishu: HMAC-SHA256 keyed by "timestamp\nsecret" over an empty message.
			msg["timestamp"] = ts
			msg["sign"] = hmacB64(ts+"\n"+cfg.Secret, "")
		}
		return postRobot(ctx, cfg.Webhook, msg)
	case "wecom":
		return postRobot(ctx, cfg.Webhook, map[string]any{"msgtype": "markdown",
			"markdown": map[string]any{"content": "**" + title + "**\n" + body}})
	case "email":
		return sendMail(ctx, cfg, title, body)
	}
	return fmt.Errorf("unsupported channel type %s", ch.Type)
}

func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

func mdLines(s string) string { return strings.ReplaceAll(s, "\n", "  \n") }

// postRobot posts JSON to a chat robot and checks its errcode/code field.
func postRobot(ctx context.Context, u string, payload any) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := notifyHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
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

func sendMail(ctx context.Context, cfg ChannelConfig, subject, body string) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: cfg.Host}
	if cfg.Port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
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
			return err
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
	msg := "From: " + cfg.From + "\r\nTo: " + strings.Join(cfg.To, ", ") +
		"\r\nSubject: " + mime.BEncoding.Encode("UTF-8", subject) +
		"\r\nDate: " + time.Now().Format(time.RFC1123Z) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		wrap76(base64.StdEncoding.EncodeToString([]byte(body)))
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
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
