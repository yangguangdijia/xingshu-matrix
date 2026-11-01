package observability

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// selectorMatches parses the small, unambiguous subset of Prometheus/Loki
// label matchers needed for exact cluster ownership. Regex and negative
// matchers do not establish ownership, even when they happen to match today.
func selectorMatches(selector, label, value string) bool {
	if !strings.HasPrefix(selector, "{") || !strings.HasSuffix(selector, "}") {
		return false
	}
	contents := selector[1 : len(selector)-1]
	var fields []string
	start, quoted, escaped := 0, false, false
	for i, ch := range contents {
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && quoted {
			escaped = true
			continue
		}
		if ch == '"' {
			quoted = !quoted
		}
		if ch == ',' && !quoted {
			fields = append(fields, contents[start:i])
			start = i + 1
		}
	}
	if quoted || escaped {
		return false
	}
	fields = append(fields, contents[start:])
	found := false
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		index := strings.IndexAny(field, "=!~")
		if index < 0 {
			return false
		}
		key := strings.TrimSpace(field[:index])
		if !clusterLabelName.MatchString(key) {
			return false
		}
		rest, operator := field[index:], ""
		for _, candidate := range []string{"=~", "!~", "!=", "="} {
			if strings.HasPrefix(rest, candidate) {
				operator = candidate
				rest = rest[len(candidate):]
				break
			}
		}
		if operator == "" {
			return false
		}
		var parsed string
		if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &parsed); err != nil {
			return false
		}
		if key == label {
			if found || operator != "=" || parsed != value {
				return false
			}
			found = true
		}
	}
	return found
}

func isIdentifier(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == ':'
}

// scopedPromQL deliberately rejects unsupported syntax rather than forwarding
// a possibly unscoped vector. Full PromQL rewriting requires a real parser.
func scopedPromQL(query string, scope sourceScope, selectorOnly bool) error {
	if !scope.Central {
		return nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return httpx.Validation("central Prometheus queries require an exact cluster selector")
	}
	if selectorOnly {
		start := strings.IndexByte(query, '{')
		if start < 0 || !strings.HasSuffix(query, "}") || !selectorMatches(query[start:], scope.Label, scope.Value) {
			return httpx.Validation("match[] requires an exact current-cluster label matcher")
		}
		for _, ch := range query[:start] {
			if !isIdentifier(ch) {
				return httpx.Validation("match[] must be a single vector selector")
			}
		}
		return nil
	}
	seen := false
	for i := 0; i < len(query); {
		switch query[i] {
		case '"', '\'':
			quote := query[i]
			i++
			closed := false
			for i < len(query) {
				if query[i] == '\\' {
					i += 2
					continue
				}
				if query[i] == quote {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return httpx.Validation("invalid PromQL string")
			}
		case '{':
			end := strings.IndexByte(query[i:], '}')
			if end < 0 || !selectorMatches(query[i:i+end+1], scope.Label, scope.Value) {
				return httpx.Validation("every vector selector must include the exact current-cluster matcher")
			}
			seen = true
			i += end + 1
		default:
			if query[i] >= '0' && query[i] <= '9' {
				for i < len(query) && (isIdentifier(rune(query[i])) || query[i] == '.') {
					i++
				}
				continue
			}
			if !isIdentifier(rune(query[i])) {
				i++
				continue
			}
			start := i
			for i < len(query) && isIdentifier(rune(query[i])) {
				i++
			}
			word := query[start:i]
			j := i
			for j < len(query) && query[j] == ' ' {
				j++
			}
			if j < len(query) && (query[j] == '{' || query[j] == '(') {
				continue
			}
			switch word {
			case "by", "without", "on", "ignoring", "group_left", "group_right", "offset", "bool", "and", "or", "unless", "Inf", "NaN":
				continue
			}
			return httpx.Validation("central PromQL requires explicit cluster matchers on all metrics")
		}
	}
	if !seen {
		return httpx.Validation("central PromQL requires a scoped vector selector")
	}
	return nil
}

func scopedLogQL(query string, scope sourceScope) error {
	if !scope.Central {
		return nil
	}
	query = strings.TrimSpace(query)
	if !strings.HasPrefix(query, "{") {
		return httpx.Validation("central LogQL requires a current-cluster stream selector")
	}
	end := strings.IndexByte(query, '}')
	if end < 0 || !selectorMatches(query[:end+1], scope.Label, scope.Value) {
		return httpx.Validation("central LogQL requires an exact current-cluster label matcher")
	}
	// This endpoint returns log streams, not LogQL binary/metric expressions.
	if strings.ContainsAny(query[end+1:], "{}") {
		return httpx.Validation("multiple LogQL selectors are not supported for central sources")
	}
	return nil
}
