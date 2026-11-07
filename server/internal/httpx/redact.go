package httpx

import (
	"net/url"
	"strings"
)

// sensitiveQueryKeys are query parameters whose values must never reach logs.
var sensitiveQueryKeys = map[string]bool{
	"access_token":  true,
	"refresh_token": true,
	"token":         true,
	"ticket":        true,
	"id_token":      true,
	"code":          true,
	"password":      true,
	"secret":        true,
	"signature":     true,
	"sig":           true,
}

// IsSensitiveQueryKey reports whether a query parameter must be redacted.
func IsSensitiveQueryKey(k string) bool { return sensitiveQueryKeys[strings.ToLower(k)] }

// RedactQuery returns raw with the values of sensitive parameters replaced by
// "***". It keeps parameter order and leaves other values untouched. An
// unparsable query is fully redacted rather than logged.
func RedactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		if p == "" {
			continue
		}
		k, _, hasVal := strings.Cut(p, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			return "***"
		}
		if IsSensitiveQueryKey(dk) && hasVal {
			parts[i] = k + "=***"
		}
	}
	return strings.Join(parts, "&")
}
