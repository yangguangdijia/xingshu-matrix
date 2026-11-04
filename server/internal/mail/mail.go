// Package mail 是平台系统邮件（SMTP）。配置存放在设置项 mail.smtp（整体加密）。
// 用于找回密码等系统通知；告警邮件仍走可观测模块的通知渠道。
package mail

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SettingKey 是保存 SMTP 配置的设置项。
const SettingKey = "mail.smtp"

// Config 是 SMTP 服务器配置。
type Config struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`     // 465 为隐式 TLS；其它端口在服务器支持时使用 STARTTLS
	Username string `json:"username"` // 留空表示不认证
	Password string `json:"password,omitempty"`
	From     string `json:"from"`     // 发件地址，可带名称：星枢 Matrix <noreply@example.com>
	Insecure bool   `json:"insecure"` // 允许在无 TLS 的连接上发信（仅内网中继），默认拒绝
	// SiteURL 是用户访问平台的地址（例如 https://matrix.example.com），用于邮件里的链接。
	// 必须由管理员配置，不能取自请求头，否则可被伪造成钓鱼链接。
	SiteURL string `json:"site_url"`
}

// Validate 检查启用所需字段。
func (c *Config) Validate() error {
	c.Host = strings.TrimSpace(c.Host)
	c.From = strings.TrimSpace(c.From)
	if !c.Enabled {
		return nil
	}
	if c.Host == "" {
		return errors.New("host is required")
	}
	if c.Port == 0 {
		c.Port = 587
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("port must be 1-65535")
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return errors.New("from must be a valid email address")
	}
	c.SiteURL = strings.TrimRight(strings.TrimSpace(c.SiteURL), "/")
	if c.SiteURL != "" && !strings.HasPrefix(c.SiteURL, "https://") && !strings.HasPrefix(c.SiteURL, "http://") {
		return errors.New("site_url must start with http:// or https://")
	}
	return nil
}

// Send 发送一封纯文本邮件。
func Send(ctx context.Context, cfg Config, to []string, subject, body string) error {
	if !cfg.Enabled || cfg.Host == "" {
		return errors.New("系统邮件未配置")
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return fmt.Errorf("发件地址不合法: %w", err)
	}
	for _, t := range to {
		if _, err := mail.ParseAddress(t); err != nil {
			return fmt.Errorf("收件地址不合法: %s", t)
		}
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if cfg.Port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
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
	secure := cfg.Port == 465
	if !secure {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("STARTTLS 失败: %w", err)
			}
			secure = true
		}
	}
	if !secure && !cfg.Insecure {
		return errors.New("SMTP 服务器不支持 TLS；如确为内网中继，请勾选「允许不加密连接」")
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	for _, t := range to {
		a, _ := mail.ParseAddress(t)
		if err := c.Rcpt(a.Address); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(Build(cfg.From, to, subject, body, time.Now()))); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Build 生成 RFC 5322 邮件文本（UTF-8 正文 base64 编码，主题 B 编码）。
func Build(from string, to []string, subject, body string, now time.Time) string {
	clean := func(s string) string { return strings.NewReplacer("\r", "", "\n", "").Replace(s) }
	var b strings.Builder
	b.WriteString("From: " + clean(from) + "\r\nTo: " + clean(strings.Join(to, ", ")) +
		"\r\nSubject: " + mime.BEncoding.Encode("UTF-8", clean(subject)) +
		"\r\nDate: " + now.Format(time.RFC1123Z) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
}
