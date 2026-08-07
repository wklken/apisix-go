// Package limitbase provides shared helpers for the rate-limit plugin family:
// limit-req, limit-conn, limit-count, and graphql-limit-count.
package limitbase

import (
	"regexp"
	"strconv"
)

// VarPattern matches $name and ${name} variable references in key and value
// expressions used by limit-req and limit-conn.
var VarPattern = regexp.MustCompile(`\$\{([0-9A-Za-z_]+)\}|\$([0-9A-Za-z_]+)`)

// DefaultVarPattern matches ${name ?? default} references with an inline
// fallback value used by limit-conn and limit-count.
var DefaultVarPattern = regexp.MustCompile(`^\$\{\s*([0-9A-Za-z_]+)\s*\?\?\s*([^{}]+?)\s*\}$`)

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
