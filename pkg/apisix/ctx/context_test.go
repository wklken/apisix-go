package ctx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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

func TestReadRequestBodyWithLimitCachesReadError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", strings.NewReader("hello!"))
	req = WithRequestVars(req)

	firstBody, firstErr := ReadRequestBodyWithLimit(req, 5)
	secondBody, secondErr := ReadRequestBodyWithLimit(req, 5)

	var firstMaxBytesErr *http.MaxBytesError
	var secondMaxBytesErr *http.MaxBytesError
	if !errors.As(firstErr, &firstMaxBytesErr) {
		t.Fatalf("first ReadRequestBodyWithLimit() error = %v, want *http.MaxBytesError", firstErr)
	}
	if !errors.As(secondErr, &secondMaxBytesErr) {
		t.Fatalf("second ReadRequestBodyWithLimit() error = %v, want *http.MaxBytesError", secondErr)
	}
	if !reflect.DeepEqual(firstBody, secondBody) {
		t.Fatalf("cached body = %q, want %q", secondBody, firstBody)
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

var errSentinelBodyRead = errors.New("sentinel body read error")

type sentinelErrorReader struct {
	err error
}

func (r sentinelErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestRequestBodyErrorCachedOnRepeatedMaxBytesRead(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", strings.NewReader("hello!"))
	first, firstErr := ReadRequestBodyWithLimit(req, 5)
	second, secondErr := ReadRequestBodyWithLimit(req, 5)

	var firstMaxBytes *http.MaxBytesError
	if !errors.As(firstErr, &firstMaxBytes) {
		t.Fatalf("first error = %v, want *http.MaxBytesError", firstErr)
	}
	var secondMaxBytes *http.MaxBytesError
	if !errors.As(secondErr, &secondMaxBytes) {
		t.Fatalf("second error = %v, want cached *http.MaxBytesError", secondErr)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("first body = %q, second body = %q; want matching partial bytes", first, second)
	}
}

func TestRequestBodyErrorCachedOnRepeatedReaderFailure(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", nil)
	req.Body = io.NopCloser(io.MultiReader(
		strings.NewReader("partial"),
		sentinelErrorReader{err: errSentinelBodyRead},
	))
	first, firstErr := ReadRequestBodyWithLimit(req, 1024)
	second, secondErr := ReadRequestBodyWithLimit(req, 1024)

	if !errors.Is(firstErr, errSentinelBodyRead) {
		t.Fatalf("first error = %v, want sentinel read error", firstErr)
	}
	if !errors.Is(secondErr, errSentinelBodyRead) {
		t.Fatalf("second error = %v, want cached sentinel read error", secondErr)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("first body = %q, second body = %q; want matching partial bytes", first, second)
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

func TestRegisterRequestVarWithoutRequestStateIsNoOp(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	RegisterRequestVar(req, "$status", http.StatusOK)
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
	req = WithBeforeProxyHook(req, func(r *http.Request) error {
		calls = append(calls, "first:"+r.URL.Path)
		return nil
	})
	req = WithBeforeProxyHook(req, func(r *http.Request) error {
		calls = append(calls, "second:"+r.URL.Path)
		return nil
	})
	req.URL.Path = "/final"

	if err := RunBeforeProxyHooks(req); err != nil {
		t.Fatalf("RunBeforeProxyHooks() error = %v", err)
	}
	if err := RunBeforeProxyHooks(req); err != nil {
		t.Fatalf("repeat RunBeforeProxyHooks() error = %v", err)
	}

	if got, want := calls, []string{"first:/final", "second:/final"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hook calls = %#v, want %#v", got, want)
	}
}

func TestBeforeProxyHooksStopAtFirstErrorAndRepeatIt(t *testing.T) {
	stopErr := errors.New("stop")
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	calls := 0
	req = WithBeforeProxyHook(req, func(*http.Request) error {
		calls++
		return stopErr
	})
	req = WithBeforeProxyHook(req, func(*http.Request) error {
		calls++
		return nil
	})

	if err := RunBeforeProxyHooks(req); !errors.Is(err, stopErr) {
		t.Fatalf("RunBeforeProxyHooks() error = %v, want stop", err)
	}
	if err := RunBeforeProxyHooks(req); !errors.Is(err, stopErr) {
		t.Fatalf("repeat error = %v, want stop", err)
	}
	if calls != 1 {
		t.Fatalf("hook calls = %d, want 1", calls)
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

func TestAuthenticationStateFromReturnsIndependentCopies(t *testing.T) {
	consumer := resource.Consumer{
		Username: "alice",
		GroupID:  "group-a",
		Plugins: map[string]resource.PluginConfig{
			"jwt-auth": map[string]any{
				"claims": map[string]any{
					"roles": []any{"reader", map[string]any{"name": "nested"}},
				},
			},
		},
		Labels: map[string]any{
			"team":  "edge",
			"zones": []string{"a", "b"},
		},
	}
	state := NewAuthenticationState("jwt-auth", consumer)
	request := WithAuthenticationState(httptest.NewRequest(http.MethodGet, "/", nil), state)
	stateCopy := state.Consumer()
	stateCopy.Plugins["jwt-auth"].(map[string]any)["claims"].(map[string]any)["roles"].([]any)[0] = "mutated-state-copy"

	got, ok := AuthenticationStateFrom(request)
	if !ok {
		t.Fatal("AuthenticationStateFrom() = false, want true")
	}
	gotConsumer := got.Consumer()
	gotConsumer.Plugins["jwt-auth"].(map[string]any)["claims"].(map[string]any)["roles"].([]any)[1].(map[string]any)["name"] = "mutated"
	gotConsumer.Labels["zones"].([]string)[0] = "mutated"

	if gotName := consumer.Plugins["jwt-auth"].(map[string]any)["claims"].(map[string]any)["roles"].([]any)[1].(map[string]any)["name"]; gotName != "nested" {
		t.Fatalf("original nested plugin value = %q, want nested", gotName)
	}
	if gotZone := consumer.Labels["zones"].([]string)[0]; gotZone != "a" {
		t.Fatalf("original label slice value = %q, want a", gotZone)
	}

	consumer.Plugins["jwt-auth"].(map[string]any)["claims"].(map[string]any)["roles"].([]any)[0] = "changed-after-store"
	consumer.Labels["team"] = "changed-after-store"
	stored, _ := AuthenticationStateFrom(request)
	storedConsumer := stored.Consumer()
	if gotRole := storedConsumer.Plugins["jwt-auth"].(map[string]any)["claims"].(map[string]any)["roles"].([]any)[0]; gotRole != "reader" {
		t.Fatalf("stored auth state role = %q, want reader", gotRole)
	}
	if gotTeam := storedConsumer.Labels["team"]; gotTeam != "edge" {
		t.Fatalf("stored auth state label = %q, want edge", gotTeam)
	}
	if got.Source != "jwt-auth" {
		t.Fatalf("authentication source = %q, want jwt-auth", got.Source)
	}
}

func TestAuthenticationProbeStartsWithoutPublishedState(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://example.com/original?one=1", strings.NewReader("payload"))
	request.Header.Set("X-Probe", "original")
	request = WithAuthenticationState(request, NewAuthenticationState("key-auth", resource.Consumer{Username: "alice"}))
	request = WithAuthProbeDiagnosticRecorder(request, func(string) {})

	probe := NewAuthenticationProbeRequest(request)
	probe.Header.Set("X-Probe", "probe")
	probe.URL.Path = "/probe"
	probe.URL.RawQuery = "two=2"

	if got := request.Header.Get("X-Probe"); got != "original" {
		t.Fatalf("original header = %q, want original", got)
	}
	if got := request.URL.RequestURI(); got != "/original?one=1" {
		t.Fatalf("original URI = %q, want /original?one=1", got)
	}
	if _, ok := AuthenticationStateFrom(probe); ok {
		t.Fatal("probe unexpectedly retained authentication state")
	}
	if RecordAuthProbeDiagnostic(probe, "losing probe") {
		t.Fatal("probe unexpectedly retained diagnostics recorder")
	}
	if probe.Body != http.NoBody {
		t.Fatalf("probe body = %T, want http.NoBody until multi-auth installs a bounded replay", probe.Body)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil || string(body) != "payload" {
		t.Fatalf("parent body = %q, error = %v; want unread payload", body, err)
	}
}

func TestTypedContextGettersReturnValuesAndZeroOnMismatch(t *testing.T) {
	ctx := withTestValue(context.Background(), "string", "value")

	if got := GetString(ctx, "string"); got != "value" {
		t.Fatalf("GetString() = %q", got)
	}
	if got := GetString(context.Background(), "absent"); got != "" {
		t.Fatalf("GetString(absent) = %q, want empty", got)
	}
	mismatch := withTestValue(context.Background(), "count", 7)
	if got := GetString(mismatch, "count"); got != "" {
		t.Fatalf("GetString(type mismatch) = %q, want empty", got)
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
