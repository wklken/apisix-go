package limitbase

import (
	"regexp"
	"testing"
)

func TestRedisInt(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int64
		ok    bool
	}{
		{name: "int", value: int(1), want: 1, ok: true},
		{name: "negative int", value: int(-1), want: -1, ok: true},
		{name: "zero int", value: int(0), want: 0, ok: true},
		{name: "int64", value: int64(2), want: 2, ok: true},
		{name: "negative int64", value: int64(-2), want: -2, ok: true},
		{name: "uint64 in range", value: uint64(3), want: 3, ok: true},
		{name: "uint64 overflow rejected", value: ^uint64(0)},
		{name: "decimal string", value: "4", want: 4, ok: true},
		{name: "negative decimal string", value: "-5", want: -5, ok: true},
		{name: "invalid string", value: "invalid"},
		{name: "bytes rejected", value: []byte("5")},
		{name: "float rejected", value: 5.0},
		{name: "nil rejected", value: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := RedisInt(tt.value)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("RedisInt(%#v) = %d, %t; want %d, %t", tt.value, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDefaultQuotaHeaders(t *testing.T) {
	tests := []struct {
		name      string
		limit     string
		remaining string
		reset     string
		want      QuotaHeaders
	}{
		{
			name: "all empty uses defaults",
			want: QuotaHeaders{
				Limit:     "X-RateLimit-Limit",
				Remaining: "X-RateLimit-Remaining",
				Reset:     "X-RateLimit-Reset",
			},
		},
		{
			name:      "configured headers pass through",
			limit:     "X-Custom-Limit",
			remaining: "X-Custom-Remaining",
			reset:     "X-Custom-Reset",
			want:      QuotaHeaders{Limit: "X-Custom-Limit", Remaining: "X-Custom-Remaining", Reset: "X-Custom-Reset"},
		},
		{
			name:  "partial configuration keeps defaults for empty fields",
			limit: "X-Custom-Limit",
			want: QuotaHeaders{
				Limit:     "X-Custom-Limit",
				Remaining: "X-RateLimit-Remaining",
				Reset:     "X-RateLimit-Reset",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DefaultQuotaHeaders(tt.limit, tt.remaining, tt.reset)
			if got != tt.want {
				t.Fatalf(
					"DefaultQuotaHeaders(%q, %q, %q) = %+v, want %+v",
					tt.limit,
					tt.remaining,
					tt.reset,
					got,
					tt.want,
				)
			}
		})
	}
}

func TestRuleQuotaHeaders(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		index  int
		want   QuotaHeaders
	}{
		{
			name:  "empty prefix numbers the rule",
			index: 0,
			want: QuotaHeaders{
				Limit:     "X-1-RateLimit-Limit",
				Remaining: "X-1-RateLimit-Remaining",
				Reset:     "X-1-RateLimit-Reset",
			},
		},
		{
			name:  "index numbers from zero",
			index: 2,
			want: QuotaHeaders{
				Limit:     "X-3-RateLimit-Limit",
				Remaining: "X-3-RateLimit-Remaining",
				Reset:     "X-3-RateLimit-Reset",
			},
		},
		{
			name:   "configured prefix wins",
			prefix: "r",
			index:  5,
			want: QuotaHeaders{
				Limit:     "X-r-RateLimit-Limit",
				Remaining: "X-r-RateLimit-Remaining",
				Reset:     "X-r-RateLimit-Reset",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RuleQuotaHeaders(tt.prefix, tt.index)
			if got != tt.want {
				t.Fatalf("RuleQuotaHeaders(%q, %d) = %+v, want %+v", tt.prefix, tt.index, got, tt.want)
			}
		})
	}
}

func TestVarPatternsMatchReferences(t *testing.T) {
	matched := []string{"$name", "${name}", "${http_conn}", "$remote_addr"}
	for _, expr := range matched {
		if !VarPattern.MatchString(expr) {
			t.Errorf("VarPattern does not match %q", expr)
		}
	}

	defaultExpr := "${http_conn ?? 5}"
	if match := DefaultVarPattern.FindStringSubmatch(
		defaultExpr,
	); match == nil || match[1] != "http_conn" ||
		match[2] != "5" {
		t.Fatalf("DefaultVarPattern(%q) = %v, want groups [http_conn 5]", defaultExpr, match)
	}
	if match := DefaultVarPattern.FindStringSubmatch(
		"${http_conn ?? default value}",
	); match == nil || match[1] != "http_conn" ||
		match[2] != "default value" {
		t.Fatalf(
			"DefaultVarPattern(%q) = %v, want groups [http_conn default value]",
			"${http_conn ?? default value}",
			match,
		)
	}
	if DefaultVarPattern.MatchString("$name") || DefaultVarPattern.MatchString("${name}") {
		t.Error("DefaultVarPattern must only match ?? fallback expressions")
	}
}

func TestVarPatternDefinitionsEqualLegacyRegexes(t *testing.T) {
	legacyVarPattern := regexp.MustCompile(`\$\{([0-9A-Za-z_]+)\}|\$([0-9A-Za-z_]+)`)
	if legacyVarPattern.String() != VarPattern.String() {
		t.Fatalf("VarPattern = %q, want %q", VarPattern.String(), legacyVarPattern.String())
	}
	legacyDefaultVarPattern := regexp.MustCompile(`^\$\{\s*([0-9A-Za-z_]+)\s*\?\?\s*([^{}]+?)\s*\}$`)
	if legacyDefaultVarPattern.String() != DefaultVarPattern.String() {
		t.Fatalf("DefaultVarPattern = %q, want %q", DefaultVarPattern.String(), legacyDefaultVarPattern.String())
	}
}
