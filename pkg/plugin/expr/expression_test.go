package expr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

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

func TestRequestValueResolvesBuiltInHTTPVariables(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://gateway.test/orders/42?item=book", nil)
	request.RemoteAddr = "192.0.2.10:4321"
	request.Header.Set("X-Forwarded-Proto", "wss")
	request.Header.Add("X-Trace", "one")
	request.Header.Add("X-Trace", "two")
	request.AddCookie(&http.Cookie{Name: "session", Value: "cookie-value"})

	tests := []struct {
		name string
		want any
	}{
		{name: "$uri", want: "/orders/42"},
		{name: "request_uri", want: "/orders/42?item=book"},
		{name: "query_string", want: "item=book"},
		{name: "args", want: "item=book"},
		{name: "is_args", want: "?"},
		{name: "method", want: http.MethodPost},
		{name: "request_method", want: http.MethodPost},
		{name: "host", want: "gateway.test"},
		{name: "scheme", want: "wss"},
		{name: "remote_addr", want: "192.0.2.10"},
		{name: "remote_port", want: "4321"},
		{name: "arg_item", want: "book"},
		{name: "cookie_session", want: "cookie-value"},
		{name: "cookie_missing", want: ""},
		{name: "http_x_trace", want: []string{"one", "two"}},
		{name: "http_missing", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RequestValue(request, test.name); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("RequestValue(%q) = %#v, want %#v", test.name, got, test.want)
			}
		})
	}

	requestWithoutArgs := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	if got := RequestValue(requestWithoutArgs, "is_args"); got != "" {
		t.Fatalf("RequestValue(is_args) = %q, want empty", got)
	}
	if got := RequestValue(requestWithoutArgs, "scheme"); got != "http" {
		t.Fatalf("plain scheme = %q, want http", got)
	}
	requestWithoutArgs.TLS = request.TLS
	if got := RequestValue(requestWithoutArgs, "scheme"); got != "https" {
		t.Fatalf("TLS scheme = %q, want https", got)
	}
}

func TestRequestValueHonorsContextOverridesAndFallbackPrecedence(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	request.RemoteAddr = "192.0.2.10:4321"
	request = request.WithContext(context.WithValue(request.Context(), apisixctx.RemoteAddrKey, "198.51.100.8"))
	request = request.WithContext(context.WithValue(request.Context(), apisixctx.RemotePortKey, "8443"))
	request = apisixctx.WithApisixVars(request, map[string]string{
		"$custom":         "apisix",
		"$request_method": "wrong-method",
	})
	request = apisixctx.WithRequestVars(request)
	apisixctx.RegisterRequestVar(request, "$custom", "request")
	apisixctx.RegisterRequestVar(request, "$request_only", 42)

	if got := RequestValue(request, "remote_addr"); got != "198.51.100.8" {
		t.Fatalf("remote_addr override = %v, want 198.51.100.8", got)
	}
	if got := RequestValue(request, "remote_port"); got != "8443" {
		t.Fatalf("remote_port override = %v, want 8443", got)
	}
	if got := RequestValue(request, "$request_method"); got != http.MethodGet {
		t.Fatalf("built-in request method = %v, want GET", got)
	}
	if got := RequestValue(request, "$custom"); got != "apisix" {
		t.Fatalf("APISIX variable precedence = %v, want apisix", got)
	}
	if got := RequestValue(request, "$request_only"); got != 42 {
		t.Fatalf("request variable fallback = %v, want 42", got)
	}
	if got := RequestValue(request, "$missing"); got != "" {
		t.Fatalf("missing variable = %v, want empty", got)
	}
}

func TestRequestValueFallsBackToUnsplitRemoteAddressAndStringifiesCollections(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	request.RemoteAddr = "local-socket"
	if got := RequestValue(request, "remote_addr"); got != "local-socket" {
		t.Fatalf("unsplit remote_addr = %v, want local-socket", got)
	}
	if got := RequestValue(request, "remote_port"); got != "" {
		t.Fatalf("unsplit remote_port = %v, want empty", got)
	}

	if got := String([]string{"one", "two"}); got != "one,two" {
		t.Fatalf("String([]string) = %q, want one,two", got)
	}
	if got := String([]any{"one", 2}); got != "one,2" {
		t.Fatalf("String([]any) = %q, want one,2", got)
	}
	if got := String(nil); got != "" {
		t.Fatalf("String(nil) = %q, want empty", got)
	}
}
