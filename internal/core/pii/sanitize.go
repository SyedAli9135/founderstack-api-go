// Package pii scrubs sensitive values out of tool-call/LLM payloads before
// they're persisted to workflow_steps — a founder's run trace shouldn't leak
// a secret an agent happened to pass through a tool call or see in a result.
package pii

import (
	"encoding/json"
	"regexp"
	"strings"
)

const Redacted = "[REDACTED]"

// sensitiveKeys matches by substring, case-insensitive, against a JSON
// object's own key name — covers common credential/PII field-naming
// conventions across the providers this codebase talks to (Slack tokens,
// Stripe keys, OAuth secrets, etc.) without needing an exhaustive list.
var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "api_key", "apikey",
	"authorization", "auth_header", "access_token", "refresh_token",
	"private_key", "client_secret", "ssn", "social_security",
	"credit_card", "card_number", "cvv", "cvc",
}

// valuePatterns catches PII that shows up under an innocuous key name
// (e.g. a customer email inside a "notes" field) — checked against every
// string value regardless of its key.
var valuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`), // email
	regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),                            // SSN
	regexp.MustCompile(`\b(?:\d[ -]*?){13,19}\b`),                          // card-like digit run
	regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{8,}\b`),   // Stripe-style secret keys
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),                 // Slack tokens
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}\b`),                         // GitHub PATs
	regexp.MustCompile(`\bBearer\s+[A-Za-z0-9._\-]{10,}\b`),                // bearer tokens
}

// Sanitize returns a redacted copy of v (typically a tool call's args or
// result, already unmarshaled into map[string]any/[]any/etc.) — safe to
// persist as workflow_steps.input_data/output_data. v is not mutated.
func Sanitize(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, v := range val {
			if isSensitiveKey(k) {
				out[k] = Redacted
				continue
			}
			out[k] = Sanitize(v)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = Sanitize(item)
		}
		return out
	case string:
		return redactString(val)
	default:
		return v
	}
}

// SanitizeJSON runs Sanitize over raw JSON bytes, re-marshaling the result.
// Invalid or non-object/array JSON (a bare string, a tool error message,
// ...) is scanned as a string instead of erroring.
func SanitizeJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return marshalOrEmpty(redactString(string(raw)))
	}
	return marshalOrEmpty(Sanitize(v))
}

func marshalOrEmpty(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func redactString(s string) string {
	for _, re := range valuePatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}
