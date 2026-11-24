package ai

import (
	"encoding/json"
	"regexp"
	"strings"
)

// redactMask replaces every secret value.
const redactMask = "******"

var (
	// PEM private key blocks (RSA / EC / OPENSSH / ENCRYPTED / PKCS#8 ...).
	reRedactPEM = regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]*)PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
	// Authorization / Proxy-Authorization headers, keeping the scheme.
	reRedactAuthz = regexp.MustCompile(`(?i)((?:proxy-)?authorization\\?["']?\s*[:=]\s*\\?["']?\s*)(?:(bearer|basic|token|digest|negotiate|apikey)\s+)?([^\s"'\\,;]+)`)
	// scheme://user:pass@host
	reRedactURLUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://[^/\s:@"']*:)([^/\s@"']+)(@)`)
	// key = value / key: value / "key": "value" (also inside escaped JSON).
	// Group 1: preceding non-alphanumeric char (or start), 2: key, 3: separator, 4: value.
	reRedactKV = regexp.MustCompile(`(?i)(^|[^a-z0-9])((?:[a-z0-9]+[_.\-])*(?:password|passwd|passphrase|pwd|pass|token|secret|secret[_\-]?key|secret[_\-]?access[_\-]?key|access[_\-]?key(?:[_\-]?id|[_\-]?secret)?|api[_\-]?key|apikey|private[_\-]?key|client[_\-]?secret|credentials?|ak|sk))(\\?["']?\s*[:=]\s*)(\\"[^"\\]*\\"|"[^"]*"|'[^']*'|[^\s"'\\&,;}\]<>]+)`)
	// CLI flags: --password xxx, --token=xxx handled by KV; sshpass -p xxx; mysql -pxxx.
	reRedactFlag    = regexp.MustCompile(`(?i)(--(?:password|passwd|token|secret|api-key|access-key|secret-key|private-key)\s+)('[^']*'|"[^"]*"|\S+)`)
	reRedactSSHPass = regexp.MustCompile(`(?i)(\bsshpass\s+-p\s*)('[^']*'|"[^"]*"|\S+)`)
	reRedactMySQLP  = regexp.MustCompile(`(?i)(\bmysql(?:dump|admin)?\b[^\n|;&]*?\s-p)('[^']*'|"[^"]*"|[^\s'"]+)`)
	// Chinese: 密码：xxx / 密码是 xxx / 口令=xxx / 密钥为 xxx
	reRedactZH = regexp.MustCompile(`(密码|口令|密钥|私钥|令牌)(\s*(?:[:：=]|是|为)\s*)([^\s，。、,;；"'）)]+)`)
	// Well-known access-key id formats (AWS, Aliyun, Tencent, GitHub, Slack, OpenAI-style).
	reRedactKnownKeys = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\bLTAI[0-9A-Za-z]{12,30}\b|\bAKID[0-9A-Za-z]{13,40}\b|\bgh[pousr]_[0-9A-Za-z]{30,}\b|\bxox[abposr]-[0-9A-Za-z\-]{10,}\b|\bsk-[0-9A-Za-z_\-]{20,}\b`)
)

// RedactSecrets masks credentials in free text before it is sent to a model
// or stored: password=/passwd/pwd/token/secret/AK/SK key-value pairs (plain,
// YAML, JSON and escaped JSON), private key blocks, Authorization headers,
// user:pass@ in URLs, password CLI flags and well-known access-key formats.
// It is idempotent: masked text stays unchanged.
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	s = reRedactPEM.ReplaceAllString(s, "-----BEGIN ${1}PRIVATE KEY-----"+redactMask+"-----END ${1}PRIVATE KEY-----")
	s = reRedactAuthz.ReplaceAllStringFunc(s, func(m string) string {
		g := reRedactAuthz.FindStringSubmatch(m)
		if g[2] != "" {
			return g[1] + g[2] + " " + redactMask
		}
		return g[1] + redactMask
	})
	s = reRedactURLUserinfo.ReplaceAllString(s, "${1}"+redactMask+"${3}")
	s = reRedactKV.ReplaceAllStringFunc(s, func(m string) string {
		g := reRedactKV.FindStringSubmatch(m)
		return g[1] + g[2] + g[3] + maskQuoted(g[4])
	})
	s = reRedactFlag.ReplaceAllStringFunc(s, func(m string) string {
		g := reRedactFlag.FindStringSubmatch(m)
		return g[1] + maskQuoted(g[2])
	})
	s = reRedactSSHPass.ReplaceAllStringFunc(s, func(m string) string {
		g := reRedactSSHPass.FindStringSubmatch(m)
		return g[1] + maskQuoted(g[2])
	})
	s = reRedactMySQLP.ReplaceAllStringFunc(s, func(m string) string {
		g := reRedactMySQLP.FindStringSubmatch(m)
		return g[1] + maskQuoted(g[2])
	})
	s = reRedactZH.ReplaceAllString(s, "${1}${2}"+redactMask)
	s = reRedactKnownKeys.ReplaceAllString(s, redactMask)
	return s
}

// maskQuoted masks a value and keeps surrounding quotes so JSON stays valid.
func maskQuoted(v string) string {
	switch {
	case strings.HasPrefix(v, `\"`) && strings.HasSuffix(v, `\"`) && len(v) >= 4:
		return `\"` + redactMask + `\"`
	case len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0]:
		return v[:1] + redactMask + v[:1]
	}
	return redactMask
}

// RedactMessage returns a copy of m with secrets masked in its text content
// and tool-call arguments. Non-string (multi-part) content is redacted on its
// raw JSON and kept only when the result is still valid JSON.
func RedactMessage(m Message) Message {
	if len(m.Content) > 0 {
		var str string
		if json.Unmarshal(m.Content, &str) == nil {
			if r := RedactSecrets(str); r != str {
				m.Content, _ = json.Marshal(r)
			}
		} else if r := RedactSecrets(string(m.Content)); r != string(m.Content) && json.Valid([]byte(r)) {
			m.Content = json.RawMessage(r)
		}
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]ToolCall, len(m.ToolCalls))
		copy(calls, m.ToolCalls)
		for i := range calls {
			calls[i].Function.Arguments = RedactSecrets(calls[i].Function.Arguments)
		}
		m.ToolCalls = calls
	}
	return m
}

// RedactMessages applies RedactMessage to every message (the input is not modified).
func RedactMessages(ms []Message) []Message {
	out := make([]Message, len(ms))
	for i, m := range ms {
		out[i] = RedactMessage(m)
	}
	return out
}
