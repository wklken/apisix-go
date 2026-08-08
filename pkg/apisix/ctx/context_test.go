package ctx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestRequestStateSharesOneContextValueAndTypedFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = WithApisixVars(req, map[string]string{"$route_id": "route-1"})
	state := GetRequestState(req)
	if state == nil {
		t.Fatal("request state is nil")
	}
	reqWithVars := WithRequestVars(req)
	if GetRequestState(reqWithVars) != state {
		t.Fatal("WithRequestVars installed a second request state")
	}
	RegisterApisixVar(reqWithVars, "$balancer_ip", "127.0.0.1")
	RegisterApisixVar(reqWithVars, "$balancer_port", "8080")
	RegisterRequestVar(reqWithVars, "$value", "ok")
	if state.BalancerIP != "127.0.0.1" || state.BalancerPort != "8080" {
		t.Fatalf("typed balancer state = %s:%s", state.BalancerIP, state.BalancerPort)
	}
	if got := GetRequestVar(reqWithVars, "$value"); got != "ok" {
		t.Fatalf("request variable = %v, want ok", got)
	}
	RecycleVars(reqWithVars)
}

func TestReadRequestBodyWithoutRequestVars(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/missing", strings.NewReader("payload"))
	body, err := ReadRequestBody(req)
	if err != nil {
		t.Fatalf("ReadRequestBody() error = %v", err)
	}
	if string(body) != "payload" {
		t.Fatalf("body = %q, want payload", body)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil || string(restored) != "payload" {
		t.Fatalf("restored body = %q, error = %v", restored, err)
	}
}

func TestReadRequestBodyWithLimitAtBoundary(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", strings.NewReader("hello"))
	body, err := ReadRequestBodyWithLimit(req, 5)
	if err != nil {
		t.Fatalf("ReadRequestBodyWithLimit() error = %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", body)
	}
}

func TestReadRequestBodyWithLimitReportsMaxBytesError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", strings.NewReader("hello!"))
	_, err := ReadRequestBodyWithLimit(req, 5)
	var maxBytesErr *http.MaxBytesError
	if !errors.As(err, &maxBytesErr) {
		t.Fatalf("ReadRequestBodyWithLimit() error = %v, want *http.MaxBytesError", err)
	}
	if maxBytesErr.Limit != 5 {
		t.Fatalf("MaxBytesError.Limit = %d, want 5", maxBytesErr.Limit)
	}
}

func TestReadRequestBodyRejectsNonByteBodyContextValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", nil)
	req = WithRequestVars(req)
	RegisterRequestVar(req, RequestBodyKey, "not-a-body")

	if _, err := ReadRequestBody(req); err == nil {
		t.Fatal("ReadRequestBody() error = nil for a non-[]byte $request_body value")
	}
}

func TestAttachConsumerSetsUpstreamUsernameHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = WithApisixVars(req, map[string]string{})

	AttachConsumer(req, resource.Consumer{Username: "bob"})

	if got := req.Header.Get("X-Consumer-Username"); got != "bob" {
		t.Fatalf("X-Consumer-Username = %q, want bob", got)
	}
}

func TestRegisterApisixVarWithoutStateDoesNotPanic(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	RegisterApisixVar(req, "$route_id", "route-1")
}

func TestWithTrustedProxyMarksOnlyDerivedRequest(t *testing.T) {
	original := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	trusted := WithTrustedProxy(original)

	if IsTrustedProxy(original) {
		t.Fatal("original request is marked as trusted")
	}
	if !IsTrustedProxy(trusted) {
		t.Fatal("derived request is not marked as trusted")
	}
}

func TestBeforeProxyHooksRunInRegistrationOrder(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/original", nil)
	var calls []string
	req = WithBeforeProxyHook(req, func(r *http.Request) {
		calls = append(calls, "first:"+r.URL.Path)
	})
	req = WithBeforeProxyHook(req, func(r *http.Request) {
		calls = append(calls, "second:"+r.URL.Path)
	})
	req.URL.Path = "/final"

	RunBeforeProxyHooks(req)
	RunBeforeProxyHooks(req)

	if got, want := calls, []string{"first:/final", "second:/final"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hook calls = %#v, want %#v", got, want)
	}
}

func TestFinalizeProxyRewriteUpdatesMethodAndEscapedTarget(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/old?keep=0", nil)
	req = req.WithContext(context.WithValue(req.Context(), ProxyRewriteKey, map[string]any{
		"uri":    "/private/%2Fraw?token=redacted",
		"method": http.MethodPost,
		"host":   "api.example.com",
		"scheme": "https",
	}))

	rewrite := FinalizeProxyRewrite(req)

	if req.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", req.Method)
	}
	if got := req.URL.RequestURI(); got != "/private/%2Fraw?token=redacted" {
		t.Fatalf("request URI = %q, want encoded path and query", got)
	}
	if rewrite.Host != "api.example.com" || rewrite.Scheme != "https" {
		t.Fatalf("rewrite target = %#v, want host and scheme", rewrite)
	}
}

func TestRunConsumerPluginsUsesRegisteredRunner(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	called := false
	req = WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		called = true
		next.ServeHTTP(w, r)
	})
	response := httptest.NewRecorder()

	RunConsumerPlugins(response, req, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	if !called {
		t.Fatal("consumer plugin runner was not called")
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRunConsumerPluginsFallsBackToNextHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	response := httptest.NewRecorder()

	RunConsumerPlugins(response, req, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRunConsumerPluginsRunsRunnerOnceAcrossStackedAuthCalls(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	runnerCalls := 0
	req = WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		runnerCalls++
		next.ServeHTTP(w, r)
	})
	downstreamCalls := 0
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	secondAuth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		RunConsumerPlugins(w, r, downstream)
	})
	response := httptest.NewRecorder()

	RunConsumerPlugins(response, req, secondAuth)

	if runnerCalls != 1 {
		t.Fatalf("consumer runner calls = %d, want 1", runnerCalls)
	}
	if downstreamCalls != 1 {
		t.Fatalf("downstream calls = %d, want 1", downstreamCalls)
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestAuthProbeDiagnosticRecorderIsRequestScoped(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	var diagnostics []string
	if RecordAuthProbeDiagnostic(req, "outside") {
		t.Fatal("RecordAuthProbeDiagnostic() recorded without a request recorder")
	}

	req = WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	if !RecordAuthProbeDiagnostic(req, "first") || !RecordAuthProbeDiagnostic(req, "second") {
		t.Fatal("RecordAuthProbeDiagnostic() did not use the installed recorder")
	}
	if !reflect.DeepEqual(diagnostics, []string{"first", "second"}) {
		t.Fatalf("diagnostics = %v, want [first second]", diagnostics)
	}
}

func TestTypedContextGettersReturnValuesAndZeroOnMismatch(t *testing.T) {
	now := time.Date(2026, time.August, 7, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	ctx = withTestValue(ctx, "string", "value")
	ctx = withTestValue(ctx, "int", 7)
	ctx = withTestValue(ctx, "int64", int64(8))
	ctx = withTestValue(ctx, "bool", true)
	ctx = withTestValue(ctx, "bytes", []byte("raw"))
	ctx = withTestValue(ctx, "map-string-string", map[string]string{"k": "v"})
	ctx = withTestValue(ctx, "map-string-any", map[string]any{"k": 1})
	ctx = withTestValue(ctx, "slice-string", []string{"a", "b"})
	ctx = withTestValue(ctx, "time", now)
	ctx = withTestValue(ctx, "duration", 5*time.Second)

	if got := GetString(ctx, "string"); got != "value" {
		t.Fatalf("GetString() = %q", got)
	}
	if got := GetInt(ctx, "int"); got != 7 {
		t.Fatalf("GetInt() = %d", got)
	}
	if got := GetInt64(ctx, "int64"); got != 8 {
		t.Fatalf("GetInt64() = %d", got)
	}
	if got := GetBool(ctx, "bool"); !got {
		t.Fatal("GetBool() = false")
	}
	if got := GetBytes(ctx, "bytes"); string(got) != "raw" {
		t.Fatalf("GetBytes() = %q", got)
	}
	if got := GetMapStringString(ctx, "map-string-string"); got["k"] != "v" {
		t.Fatalf("GetMapStringString() = %v", got)
	}
	if got := GetMapStringAny(ctx, "map-string-any"); got["k"] != 1 {
		t.Fatalf("GetMapStringAny() = %v", got)
	}
	if got := GetSliceString(ctx, "slice-string"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("GetSliceString() = %v", got)
	}
	if got := GetTime(ctx, "time"); !got.Equal(now) {
		t.Fatalf("GetTime() = %v", got)
	}
	if got := GetDuration(ctx, "duration"); got != 5*time.Second {
		t.Fatalf("GetDuration() = %v", got)
	}

	mismatch := withTestValue(context.Background(), "int", "not-an-int")
	if got := GetInt(mismatch, "int"); got != 0 {
		t.Fatalf("GetInt(type mismatch) = %d, want 0", got)
	}
	if got := GetString(context.Background(), "absent"); got != "" {
		t.Fatalf("GetString(absent) = %q, want empty", got)
	}
	if got := GetDuration(mismatch, "int"); got != 0 {
		t.Fatalf("GetDuration(type mismatch) = %v, want 0", got)
	}
}

func TestWithConsumerPluginOverridesScopesNames(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	if ConsumerPluginOverrides(request, "key-auth") {
		t.Fatal("override present without WithConsumerPluginOverrides")
	}

	request = WithConsumerPluginOverrides(request, map[string]struct{}{"key-auth": {}})
	if !ConsumerPluginOverrides(request, "key-auth") {
		t.Fatal("override missing after WithConsumerPluginOverrides")
	}
	if ConsumerPluginOverrides(request, "jwt-auth") {
		t.Fatal("override present for an unlisted plugin")
	}
}

//nolint:staticcheck // the production getters look up plain string keys
func withTestValue(parent context.Context, key string, value any) context.Context {
	return context.WithValue(parent, key, value)
}
