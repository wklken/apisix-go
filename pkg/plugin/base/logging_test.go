package base

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestReadRequestBodyRestoresRequestBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload"))

	body, err := ReadRequestBody(r)
	if err != nil {
		t.Fatalf("ReadRequestBody() error = %v", err)
	}
	if string(body) != "payload" {
		t.Fatalf("returned body = %q, want payload", body)
	}
	restored, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != "payload" {
		t.Fatalf("restored body = %q, want payload", restored)
	}
}

func TestWriteJSONMessageUsesProjectJSONContract(t *testing.T) {
	rr := httptest.NewRecorder()

	WriteJSONMessage(rr, http.StatusBadRequest, "bad \"<input>\" & line\nnext")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("content type = %q, want application/json with UTF-8 charset", got)
	}
	if got := rr.Body.String(); got != `{"message":"bad \"\u003cinput\u003e\" \u0026 line\nnext"}` {
		t.Fatalf("body = %q, want project JSON escaping without a trailing newline", got)
	}
}

func TestRemoteIPStripsPortWhenPresent(t *testing.T) {
	if got := RemoteIP("192.0.2.10:8080"); got != "192.0.2.10" {
		t.Fatalf("RemoteIP() = %q, want 192.0.2.10", got)
	}
	if got := RemoteIP("192.0.2.10"); got != "192.0.2.10" {
		t.Fatalf("RemoteIP() without port = %q, want original address", got)
	}
}

func TestRequestVarFromNginxSupportsHeadersAndRemoteAddress(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.10:8080"
	r.Header.Set("X-Request-Id", "request-123")

	if got := RequestVarFromNginx(r, "$remote_addr"); got != "192.0.2.10" {
		t.Fatalf("remote_addr = %q, want 192.0.2.10", got)
	}
	if got := RequestVarFromNginx(r, "$http_x_request_id"); got != "request-123" {
		t.Fatalf("http_x_request_id = %q, want request-123", got)
	}
}

func TestRequestVarReadsApisixContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = apisixctx.WithApisixVars(r, map[string]string{"$route_id": "route-1"})

	if got := RequestVar(r, "$route_id", 0); got != "route-1" {
		t.Fatalf("RequestVar() = %q, want route-1", got)
	}
}

func TestExprMatchedSupportsBothPluginExpressionShapes(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/items?id=42", nil)
	r.Header.Set("X-Trace-Id", "abc-123")

	tests := []struct {
		name        string
		expressions any
	}{
		{
			name: "flat expressions",
			expressions: []any{
				[]any{"$arg_id", "==", "42"},
				"AND",
				[]any{"$http_x_trace_id", "~", "^abc"},
			},
		},
		{
			name: "nested expressions",
			expressions: [][]any{
				{"$arg_id", "==", "42"},
				{"AND"},
				{"$http_x_trace_id", "~", "^abc"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := PrepareExprRegexps(test.expressions); err != nil {
				t.Fatalf("PrepareExprRegexps() error = %v", err)
			}
			if !ExprMatched(r, test.expressions, 0) {
				t.Fatalf("ExprMatched() = false, want true")
			}
		})
	}
}

func TestPrepareExprRegexpsSupportsBothRegexOperators(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/items", nil)
	r.Header.Set("X-Trace-Id", "abc-123")
	expressions := []any{
		[]any{"$http_x_trace_id", "~", "^abc-[0-9]+$"},
		"AND",
		[]any{"$http_x_trace_id", "!~", "^xyz"},
	}

	if err := PrepareExprRegexps(expressions); err != nil {
		t.Fatalf("PrepareExprRegexps() error = %v", err)
	}
	if _, ok := preparedExprRegexps.Load("^abc-[0-9]+$"); !ok {
		t.Fatal("PrepareExprRegexps() did not cache the configured pattern")
	}
	if !ExprMatched(r, expressions, 0) {
		t.Fatal("ExprMatched() = false, want both prepared regex conditions to match")
	}
}

func TestPrepareExprRegexpsFailsInvalidPattern(t *testing.T) {
	invalid := []any{[]any{"$uri", "~", "["}}
	valid := []any{[]any{"$uri", "~", "^/hello"}}
	if err := PrepareExprRegexps(invalid); err == nil {
		t.Fatal("PrepareExprRegexps(invalid pattern) error = nil")
	}
	if err := PrepareExprRegexps(valid); err != nil {
		t.Fatalf("PrepareExprRegexps(valid) error = %v", err)
	}
	if err := PrepareExprRegexps(valid, invalid); err == nil {
		t.Fatal("PrepareExprRegexps(valid, invalid) error = nil")
	}
}

func TestExprMatchedUnpreparedPatternRetainsNoMatchBehavior(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	match := []any{[]any{"$uri", "~", "^/never-prepared$"}}
	notMatch := []any{[]any{"$uri", "!~", "^/never-prepared$"}}

	if ExprMatched(r, match, 0) {
		t.Fatal("unprepared positive regex matched, want false")
	}
	if !ExprMatched(r, notMatch, 0) {
		t.Fatal("unprepared negative regex did not match, want existing true behavior")
	}
}

func TestExprMatchedPreservesStringAndNonStringOperands(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	if !ExprMatched(r, []any{[]any{"$status", "==", "201"}}, http.StatusCreated) {
		t.Fatal("string operands did not match")
	}
	if !ExprMatched(r, []any{[]any{"$status", "==", 201}}, http.StatusCreated) {
		t.Fatal("non-string operand did not retain fmt.Sprint behavior")
	}
}

func TestExprMatchedTruthTable(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/items?id=42", nil)
	r.Header.Set("X-Trace-Id", "abc-123")

	tests := []struct {
		name        string
		expressions any
		want        bool
	}{
		{name: "nil expressions", expressions: nil, want: true},
		{name: "empty flat list", expressions: []any{}, want: true},
		{name: "unsupported expression type", expressions: "not-a-list", want: false},
		{name: "exact equality match", expressions: []any{[]any{"$arg_id", "==", "42"}}, want: true},
		{name: "exact equality mismatch", expressions: []any{[]any{"$arg_id", "==", "7"}}, want: false},
		{name: "regex positive match", expressions: []any{[]any{"$http_x_trace_id", "~", "^abc"}}, want: true},
		{name: "regex positive mismatch", expressions: []any{[]any{"$http_x_trace_id", "~", "^xyz"}}, want: false},
		{name: "regex negation match", expressions: []any{[]any{"$http_x_trace_id", "!~", "^xyz"}}, want: true},
		{name: "regex negation mismatch", expressions: []any{[]any{"$http_x_trace_id", "!~", "^abc"}}, want: false},
		{
			name:        "unknown flat operator",
			expressions: []any{[]any{"$arg_id", "==", "42"}, "XOR", []any{"$uri", "==", "/"}},
			want:        false,
		},
		{
			name:        "unknown nested operator",
			expressions: [][]any{{"$arg_id", "==", "42"}, {"XOR"}, {"$uri", "==", "/"}},
			want:        false,
		},
		{name: "malformed condition", expressions: []any{"$uri"}, want: false},
		{
			name:        "or combines alternatives",
			expressions: []any{[]any{"$arg_id", "==", "7"}, "OR", []any{"$arg_id", "==", "42"}},
			want:        true,
		},
		{
			name:        "and requires both",
			expressions: []any{[]any{"$arg_id", "==", "42"}, "AND", []any{"$uri", "==", "/missing"}},
			want:        false,
		},
		{
			name:        "leading operator before first operand",
			expressions: []any{"OR", []any{"$arg_id", "==", "42"}},
			want:        true,
		},
		{
			name: "chained or resets to and",
			expressions: []any{
				[]any{"$arg_id", "==", "42"},
				"OR",
				[]any{"$arg_id", "==", "7"},
				"AND",
				[]any{"$uri", "==", "/items"},
			},
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := PrepareExprRegexps(test.expressions); err != nil {
				t.Fatalf("PrepareExprRegexps() error = %v", err)
			}
			if got := ExprMatched(r, test.expressions, 0); got != test.want {
				t.Fatalf("ExprMatched() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestApplyBatchDefaults(t *testing.T) {
	d := BatchDefaults{}
	ApplyBatchDefaults(&d)
	if d.BatchMaxSize != logger_batch.DefaultBatchMaxSize {
		t.Fatalf("BatchMaxSize = %d, want %d", d.BatchMaxSize, logger_batch.DefaultBatchMaxSize)
	}
	if d.RetryDelaySec != int(logger_batch.DefaultRetryDelay/time.Second) {
		t.Fatalf("RetryDelaySec = %d, want %d", d.RetryDelaySec, int(logger_batch.DefaultRetryDelay/time.Second))
	}
	if d.BufferDurationSec != int(logger_batch.DefaultBufferDuration/time.Second) {
		t.Fatalf(
			"BufferDurationSec = %d, want %d",
			d.BufferDurationSec,
			int(logger_batch.DefaultBufferDuration/time.Second),
		)
	}
	if d.InactiveTimeoutSec != int(logger_batch.DefaultInactiveTimeout/time.Second) {
		t.Fatalf(
			"InactiveTimeoutSec = %d, want %d",
			d.InactiveTimeoutSec,
			int(logger_batch.DefaultInactiveTimeout/time.Second),
		)
	}
	if d.MaxPendingEntries != logger_batch.DefaultMaxPendingEntries {
		t.Fatalf("MaxPendingEntries = %d, want %d", d.MaxPendingEntries, logger_batch.DefaultMaxPendingEntries)
	}
	if d.MaxConcurrentDeliveries != logger_batch.DefaultMaxConcurrentDeliveries {
		t.Fatalf(
			"MaxConcurrentDeliveries = %d, want %d",
			d.MaxConcurrentDeliveries,
			logger_batch.DefaultMaxConcurrentDeliveries,
		)
	}
	if d.DeliveryTimeoutSec != int(logger_batch.DefaultDeliveryTimeout/time.Second) {
		t.Fatalf(
			"DeliveryTimeoutSec = %d, want %d",
			d.DeliveryTimeoutSec,
			int(logger_batch.DefaultDeliveryTimeout/time.Second),
		)
	}
	if d.ShutdownTimeoutSec != int(logger_batch.DefaultShutdownTimeout/time.Second) {
		t.Fatalf(
			"ShutdownTimeoutSec = %d, want %d",
			d.ShutdownTimeoutSec,
			int(logger_batch.DefaultShutdownTimeout/time.Second),
		)
	}

	d = BatchDefaults{RetryDelaySec: 5, RetryDelaySet: false}
	ApplyBatchDefaults(&d)
	if d.RetryDelaySec != 5 {
		t.Fatalf("RetryDelaySec = %d, want 5 (explicit value preserved)", d.RetryDelaySec)
	}

	d = BatchDefaults{RetryDelaySet: true}
	ApplyBatchDefaults(&d)
	if d.RetryDelaySec != 0 {
		t.Fatalf("RetryDelaySec = %d, want 0 (set flag preserves zero)", d.RetryDelaySec)
	}

	d = BatchDefaults{BatchMaxSize: 42}
	ApplyBatchDefaults(&d)
	if d.BatchMaxSize != 42 {
		t.Fatalf("BatchMaxSize = %d, want 42 (explicit value preserved)", d.BatchMaxSize)
	}

	d = BatchDefaults{
		BatchMaxSize:            42,
		MaxPendingEntries:       43,
		PluginID:                "http-logger",
		MaxConcurrentDeliveries: 3,
		DeliveryTimeoutSec:      7,
		ShutdownTimeoutSec:      8,
	}
	ApplyBatchDefaults(&d)
	if d.BatchMaxSize != 42 || d.MaxPendingEntries != 43 || d.PluginID != "http-logger" ||
		d.MaxConcurrentDeliveries != 3 || d.DeliveryTimeoutSec != 7 || d.ShutdownTimeoutSec != 8 {
		t.Fatalf("explicit resource overrides changed: %+v", d)
	}
}

func TestNewBatchProcessorDeliversPushedEntry(t *testing.T) {
	var delivered []map[string]any
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/base-constructor", runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewBatchProcessor("test logger", owner, BatchDefaults{}, "route-1", "server-1",
		func(_ context.Context, entries []map[string]any, _ int) (int, error) {
			delivered = append(delivered, entries...)
			return 0, nil
		})
	if err != nil {
		t.Fatal(err)
	}

	if !processor.Push(map[string]any{"message": "hello"}) {
		t.Fatal("Push() = false, want true")
	}
	processor.Stop()
	if residuals, stopErr := tasks.Stop(context.Background()); stopErr != nil {
		t.Fatalf("TaskRegistry.Stop() residuals=%v error=%v", residuals, stopErr)
	}

	if len(delivered) != 1 {
		t.Fatalf("delivered %d entries, want 1", len(delivered))
	}
	if got := delivered[0]["message"]; got != "hello" {
		t.Fatalf("delivered message = %v, want hello", got)
	}
}

func TestCompareNumber(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		op    string
		want  bool
	}{
		{name: "greater", left: "3", right: "2", op: ">", want: true},
		{name: "not greater", left: "2", right: "3", op: ">"},
		{name: "greater or equal", left: "3", right: "3", op: ">=", want: true},
		{name: "less", left: "2", right: "3", op: "<", want: true},
		{name: "less or equal", left: "3", right: "3", op: "<=", want: true},
		{name: "invalid left", left: "x", right: "3", op: ">"},
		{name: "invalid right", left: "3", right: "x", op: ">"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := compareNumber(test.left, test.right, func(a, b float64) bool {
				switch test.op {
				case ">":
					return a > b
				case ">=":
					return a >= b
				case "<":
					return a < b
				default:
					return a <= b
				}
			})
			if got != test.want {
				t.Fatalf("compareNumber(%q, %q) = %t, want %t", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestNestedLogMapReturnsExistingOrCreates(t *testing.T) {
	fields := map[string]any{"existing": map[string]any{"k": "v"}}
	if got := NestedLogMap(fields, "existing"); got["k"] != "v" {
		t.Fatalf("NestedLogMap(existing) = %v, want existing map", got)
	}
	created := NestedLogMap(fields, "created")
	created["new"] = 1
	if fields["created"].(map[string]any)["new"] != 1 {
		t.Fatal("NestedLogMap() did not install the created map into fields")
	}
}

func TestRequestHeaderValueMatchesHeaderGet(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Trace-Id", "abc-123")
	request.Header.Set("X-Simple", "simple-value")

	for name, want := range map[string]string{
		"X-Trace-Id": "abc-123",
		"x-trace-id": "abc-123",
		"X-trace-Id": "abc-123",
		"X-Simple":   "simple-value",
		"x_simple":   "simple-value",
		"X-Missing":  "",
		"":           "",
	} {
		if got := requestHeaderValue(request.Header, name); got != want {
			t.Fatalf("requestHeaderValue(%q) = %q, want %q", name, got, want)
		}
	}

	longName := strings.Repeat("X", 200)
	if got := requestHeaderValue(request.Header, longName); got != request.Header.Get(longName) {
		t.Fatalf("requestHeaderValue(long) = %q, want Header.Get = %q", got, request.Header.Get(longName))
	}
}

func TestRequestVarResolvesExpressionVariables(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://api.example.test:8443/orders?q=blue", nil)
	request.RemoteAddr = "192.0.2.20:1234"
	request.Header.Set("X-Custom", "custom-value")

	if got := RequestVar(request, "$status", 201); got != "201" {
		t.Fatalf("status = %q, want 201", got)
	}
	if got := RequestVar(request, "$status_code", 0); got != "<nil>" {
		t.Fatalf("status_code without context = %q, want <nil>", got)
	}
	if got := RequestVar(request, "$uri", 0); got != "/orders" {
		t.Fatalf("uri = %q, want /orders", got)
	}
	if got := RequestVar(request, "$request_uri", 0); got != "/orders?q=blue" {
		t.Fatalf("request_uri = %q, want /orders?q=blue", got)
	}
	if got := RequestVar(request, "$method", 0); got != http.MethodPost {
		t.Fatalf("method = %q, want POST", got)
	}
	if got := RequestVar(request, "$host", 0); got != "api.example.test:8443" {
		t.Fatalf("host = %q", got)
	}
	if got := RequestVar(request, "$scheme", 0); got != "https" {
		t.Fatalf("scheme = %q, want https", got)
	}
	request.Header.Set("X-Forwarded-Proto", "http")
	if got := RequestVar(request, "$scheme", 0); got != "http" {
		t.Fatalf("scheme with X-Forwarded-Proto = %q, want http", got)
	}
	if got := RequestVar(request, "$remote_addr", 0); got != "192.0.2.20" {
		t.Fatalf("remote_addr = %q, want 192.0.2.20", got)
	}
	if got := RequestVar(request, "$arg_q", 0); got != "blue" {
		t.Fatalf("arg_q = %q, want blue", got)
	}
	if got := RequestVar(request, "$http_x_custom", 0); got != "custom-value" {
		t.Fatalf("http_x_custom = %q, want custom-value", got)
	}
	if got := RequestVar(request, "$unknown_var", 0); got != "" {
		t.Fatalf("unknown var = %q, want empty", got)
	}
}

func TestRequestVarRedactsRegisteredQueryCredentials(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/orders?keep=yes&token=secret&token=again", nil)
	apisixctx.RegisterSensitiveQueryName(request, "token")

	if got := RequestVar(request, "$request_uri", 0); got != "/orders?keep=yes&token=***&token=***" {
		t.Fatalf("request_uri = %q", got)
	}
	if got := RequestVar(request, "$args", 0); got != "keep=yes&token=***&token=***" {
		t.Fatalf("args = %q", got)
	}
	if got := RequestVar(request, "$arg_token", 0); got != "***" {
		t.Fatalf("arg_token = %q", got)
	}
	if got := request.URL.RequestURI(); got != "/orders?keep=yes&token=secret&token=again" {
		t.Fatalf("live request URI = %q", got)
	}
}
