package automation

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	automationSensitiveKey = regexp.MustCompile(`(?i)(password|passwd|token|secret|credential|api[_-]?key|private[_-]?key|authorization|access[_-]?key|client[_-]?secret)`)
	automationSecretPair   = regexp.MustCompile(`(?i)((?:"|')?(?:password|passwd|token|secret|credential|api[_-]?key|private[_-]?key|authorization|access[_-]?key|client[_-]?secret)(?:"|')?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^,\s}\]]+)`)
	automationSecretValue  = regexp.MustCompile(`(?i)(?:password|passwd|token|secret|credential|api[_-]?key|private[_-]?key|authorization|access[_-]?key|client[_-]?secret)\s*[:=]\s*("(?:[^"]*)"|'(?:[^']*)'|secret://[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+|\{\{[^}]+\}\}|\$\{[^}]+\}|[^,\s}\]]+)`)
	automationBearer       = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
	automationURLPassword  = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^:/\s]+:)[^@/\s]+(@)`)
)

func validateAutomationSecrets(steps json.RawMessage, playbook, name, description, trigger string) error {
	if inlineAutomationSecretText(name) || inlineAutomationSecretText(description) || inlineAutomationSecretText(trigger) {
		return errors.New("workflow metadata must not contain inline secrets")
	}
	if len(steps) != 0 {
		var value any
		if err := json.Unmarshal(steps, &value); err != nil || containsAutomationLiteralSecret(value, "") {
			return errors.New("workflow steps must use secret references instead of inline secrets")
		}
	}
	if strings.TrimSpace(playbook) != "" {
		var value any
		if err := yaml.Unmarshal([]byte(playbook), &value); err != nil || containsAutomationLiteralSecret(value, "") {
			return errors.New("Ansible playbook must use secret references instead of inline secrets")
		}
	}
	return nil
}

func inlineAutomationSecretText(value string) bool {
	if automationBearer.MatchString(value) || automationURLPassword.MatchString(value) {
		return true
	}
	for _, match := range automationSecretValue.FindAllStringSubmatch(value, -1) {
		if len(match) < 2 {
			continue
		}
		secret := strings.Trim(match[1], `"'`)
		if !isAutomationReference(secret) {
			return true
		}
	}
	return false
}

func containsAutomationLiteralSecret(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			if containsAutomationLiteralSecret(child, childKey) {
				return true
			}
		}
	case map[any]any:
		for childKey, child := range typed {
			name, _ := childKey.(string)
			if containsAutomationLiteralSecret(child, name) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsAutomationLiteralSecret(child, key) {
				return true
			}
		}
	case string:
		if automationSensitiveKey.MatchString(key) {
			return !isAutomationReference(typed)
		}
		return inlineAutomationSecretText(typed)
	default:
		return automationSensitiveKey.MatchString(key) && typed != nil
	}
	return false
}

func redactAutomationText(value string) string {
	value = automationBearer.ReplaceAllString(value, "${1}[REDACTED]")
	value = automationSecretPair.ReplaceAllString(value, "${1}[REDACTED]")
	return automationURLPassword.ReplaceAllString(value, "${1}[REDACTED]${2}")
}

func redactAutomationJSON(raw json.RawMessage) json.RawMessage {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return json.RawMessage(`null`)
	}
	value = redactAutomationValue(value, "")
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return encoded
}

func redactAutomationPlaybook(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var value any
	if yaml.Unmarshal([]byte(raw), &value) != nil {
		return redactAutomationText(raw)
	}
	value = redactAutomationValue(value, "")
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return redactAutomationText(raw)
	}
	return string(encoded)
}

func redactAutomationValue(value any, key string) any {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			typed[childKey] = redactAutomationValue(child, childKey)
		}
		return typed
	case map[any]any:
		for childKey, child := range typed {
			name, _ := childKey.(string)
			typed[childKey] = redactAutomationValue(child, name)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = redactAutomationValue(child, key)
		}
		return typed
	case string:
		if automationSensitiveKey.MatchString(key) && !isAutomationReference(typed) {
			return "[REDACTED]"
		}
		return redactAutomationText(typed)
	default:
		return value
	}
}

func isAutomationReference(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "{{") || strings.HasPrefix(value, "${") ||
		strings.HasPrefix(value, "secret://")
}
