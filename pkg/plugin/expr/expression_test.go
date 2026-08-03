package expr

import "testing"

func TestExpressionEvaluatesRestyOperatorsAndNestedLogic(t *testing.T) {
	expression, err := Compile([]any{
		"AND",
		[]any{"status", ">=", 200},
		[]any{"method", "in", []any{"GET", "HEAD"}},
		[]any{"roles", "has", "admin"},
		[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
		[]any{"environment", "~*", "^prod$"},
		[]any{"skip", "!", "==", "yes"},
		[]any{
			"!OR",
			[]any{"region", "==", "blocked"},
			[]any{"status", ">=", 500},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	values := map[string]any{
		"status":      202,
		"method":      "GET",
		"roles":       []string{"viewer", "admin"},
		"remote_addr": "192.0.2.40",
		"environment": "PrOd",
		"skip":        "no",
		"region":      "allowed",
	}
	if !expression.Eval(func(name string) any { return values[name] }) {
		t.Fatal("Eval() = false, want nested expression to match")
	}
}

func TestCompileRejectsInvalidExpressions(t *testing.T) {
	tests := []struct {
		name string
		rule any
	}{
		{name: "unwrapped condition", rule: []any{"status", "==", 200}},
		{name: "unknown operator", rule: []any{[]any{"status", "bogus", 200}}},
		{name: "bad not", rule: []any{[]any{"status", "not", "==", 200}}},
		{name: "in scalar", rule: []any{[]any{"method", "in", "GET"}}},
		{name: "invalid ip", rule: []any{[]any{"remote_addr", "ipmatch", "bad-ip"}}},
		{name: "short logic", rule: []any{"AND", []any{"status", "==", 200}}},
		{name: "dangling infix", rule: []any{[]any{"status", "==", 200}, "OR"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Compile(tt.rule); err == nil {
				t.Fatalf("Compile(%v) error = nil, want invalid expression rejected", tt.rule)
			}
		})
	}
}

func TestEmptyExpressionMatches(t *testing.T) {
	expression, err := Compile([]any{})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if !expression.Eval(func(string) any { return nil }) {
		t.Fatal("Eval() = false, want empty top-level expression to match")
	}
}

func TestNegatedNumericComparisonDoesNotMatchMissingValue(t *testing.T) {
	expression, err := Compile([]any{[]any{"age", "!", "<", 18}})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if expression.Eval(func(string) any { return nil }) {
		t.Fatal("Eval() = true, want missing numeric value not to match")
	}
}
