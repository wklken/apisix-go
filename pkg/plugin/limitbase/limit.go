// Package limitbase provides shared helpers for the rate-limit plugin family:
// limit-req, limit-conn, limit-count, and graphql-limit-count.
package limitbase

import (
	"regexp"
	"strconv"
	"strings"
)

// VarPattern matches $name and ${name} variable references in key and value
// expressions used by limit-req and limit-conn.
var VarPattern = regexp.MustCompile(`\$\{([0-9A-Za-z_]+)\}|\$([0-9A-Za-z_]+)`)

// DefaultVarPattern matches ${name ?? default} references with an inline
// fallback value used by limit-conn and limit-count.
var DefaultVarPattern = regexp.MustCompile(`^\$\{\s*([0-9A-Za-z_]+)\s*\?\?\s*([^{}]+?)\s*\}$`)

// ResolveVars expands APISIX variable expressions in tpl. It supports $name,
// ${name}, dotted names, and ${name ?? default}; an escaped dollar is left
// untouched. The returned count includes only variables that resolved to a
// value or used a default, matching APISIX core.utils.resolve_var.
func ResolveVars(tpl string, lookup func(string) string) (string, int) {
	var output strings.Builder
	resolved := 0
	for index := 0; index < len(tpl); {
		if tpl[index] != '$' || (index > 0 && tpl[index-1] == '\\') {
			output.WriteByte(tpl[index])
			index++
			continue
		}

		start := index
		index++
		var expression string
		if index < len(tpl) && tpl[index] == '{' {
			end := strings.IndexByte(tpl[index+1:], '}')
			if end < 0 {
				output.WriteString(tpl[start:])
				break
			}
			end += index + 1
			expression = strings.TrimSpace(tpl[index+1 : end])
			index = end + 1
		} else {
			begin := index
			for index < len(tpl) && isAPISIXVarByte(tpl[index]) {
				index++
			}
			if begin == index {
				output.WriteByte('$')
				continue
			}
			expression = tpl[begin:index]
		}

		name := expression
		fallback := ""
		hasFallback := false
		if before, after, ok := strings.Cut(expression, "??"); ok {
			name = strings.TrimSpace(before)
			fallback = strings.TrimSpace(after)
			hasFallback = true
		}
		value := lookup(name)
		if value == "" && hasFallback {
			value = fallback
		}
		if value != "" {
			resolved++
		}
		output.WriteString(value)
	}
	return output.String(), resolved
}

func isAPISIXVarByte(value byte) bool {
	return value == '.' || value == '_' ||
		value >= '0' && value <= '9' ||
		value >= 'A' && value <= 'Z' ||
		value >= 'a' && value <= 'z'
}

// RedisInt converts a Redis wire value to int64. Accepted types are int,
// int64, uint64 within int64 range, and decimal strings. Every other type and
// an overflowing uint64 is rejected.
func RedisInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		if v > uint64(1<<63-1) {
			return 0, false
		}
		return int64(v), true
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// QuotaHeaders names the rate-limit response headers.
type QuotaHeaders struct {
	Limit     string
	Remaining string
	Reset     string
}

// DefaultQuotaHeaders returns the default quota header names, substituting the
// X-RateLimit-* names for empty fields.
func DefaultQuotaHeaders(limitHeader, remainingHeader, resetHeader string) QuotaHeaders {
	if limitHeader == "" {
		limitHeader = "X-RateLimit-Limit"
	}
	if remainingHeader == "" {
		remainingHeader = "X-RateLimit-Remaining"
	}
	if resetHeader == "" {
		resetHeader = "X-RateLimit-Reset"
	}
	return QuotaHeaders{Limit: limitHeader, Remaining: remainingHeader, Reset: resetHeader}
}

// RuleQuotaHeaders returns per-rule quota header names, numbering the rule
// index plus one when no header prefix is configured.
func RuleQuotaHeaders(prefix string, index int) QuotaHeaders {
	if prefix == "" {
		prefix = strconv.Itoa(index + 1)
	}
	return QuotaHeaders{
		Limit:     "X-" + prefix + "-RateLimit-Limit",
		Remaining: "X-" + prefix + "-RateLimit-Remaining",
		Reset:     "X-" + prefix + "-RateLimit-Reset",
	}
}
