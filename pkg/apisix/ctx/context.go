package ctx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/resource"
)

type BeforeProxyHook func(*http.Request)

type beforeProxyHooks struct {
	once  sync.Once
	hooks []BeforeProxyHook
}

// inspired by gin/context.go, but we use context.Context instead of gin.Context

type ContextKey string

const (
	ProxyRewriteKey         ContextKey = "proxy-rewrite"
	RequestIDKey            ContextKey = "request_id"
	RemoteAddrKey           ContextKey = "remote_addr"
	RemotePortKey           ContextKey = "remote_port"
	consumerPluginRunnerKey ContextKey = "consumer_plugin_runner"
	consumerPluginsRunKey   ContextKey = "consumer_plugins_run"
	authProbeDiagnosticKey  ContextKey = "auth_probe_diagnostic_recorder"
	consumerOverridesKey    ContextKey = "consumer_plugin_overrides"
	beforeProxyHooksKey     ContextKey = "before_proxy_hooks"
	trustedProxyKey         ContextKey = "trusted_proxy"
)

func WithTrustedProxy(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), trustedProxyKey, true))
}

func IsTrustedProxy(r *http.Request) bool {
	trusted, _ := r.Context().Value(trustedProxyKey).(bool)
	return trusted
}

func WithBeforeProxyHook(r *http.Request, hook BeforeProxyHook) *http.Request {
	registered, _ := r.Context().Value(beforeProxyHooksKey).(*beforeProxyHooks)
	hooks := make([]BeforeProxyHook, 0, 1)
	if registered != nil {
		hooks = append(hooks, registered.hooks...)
	}
	hooks = append(hooks, hook)
	return r.WithContext(context.WithValue(r.Context(), beforeProxyHooksKey, &beforeProxyHooks{hooks: hooks}))
}

func RunBeforeProxyHooks(r *http.Request) {
	registered, _ := r.Context().Value(beforeProxyHooksKey).(*beforeProxyHooks)
	if registered == nil {
		return
	}
	registered.once.Do(func() {
		for _, hook := range registered.hooks {
			hook(r)
		}
	})
}

type ProxyRewrite struct {
	URI    string
	Method string
	Host   string
	Scheme string
}

func FinalizeProxyRewrite(r *http.Request) ProxyRewrite {
	values, _ := r.Context().Value(ProxyRewriteKey).(map[string]any)
	rewrite := ProxyRewrite{
		URI:    stringValue(values, "uri"),
		Method: stringValue(values, "method"),
		Host:   stringValue(values, "host"),
		Scheme: stringValue(values, "scheme"),
	}
	if rewrite.URI != "" {
		applyProxyRewriteURI(r, rewrite.URI)
	}
	if rewrite.Method != "" {
		r.Method = rewrite.Method
	}
	return rewrite
}

func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func applyProxyRewriteURI(r *http.Request, uri string) {
	if parsed, err := url.ParseRequestURI(uri); err == nil && parsed.Scheme == "" && parsed.Host == "" {
		r.URL.Path = parsed.Path
		r.URL.RawPath = parsed.RawPath
		r.URL.RawQuery = parsed.RawQuery
		return
	}

	path, rawQuery, hasQuery := strings.Cut(uri, "?")
	r.URL.Path = path
	r.URL.RawPath = ""
	if hasQuery {
		r.URL.RawQuery = rawQuery
	}
}

type ConsumerPluginRunner func(http.ResponseWriter, *http.Request, http.Handler)

type AuthProbeDiagnosticRecorder func(string)

func WithAuthProbeDiagnosticRecorder(r *http.Request, recorder AuthProbeDiagnosticRecorder) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authProbeDiagnosticKey, recorder))
}

func RecordAuthProbeDiagnostic(r *http.Request, message string) bool {
	recorder, _ := r.Context().Value(authProbeDiagnosticKey).(AuthProbeDiagnosticRecorder)
	if recorder == nil {
		return false
	}
	recorder(message)
	return true
}

func WithConsumerPluginRunner(r *http.Request, runner ConsumerPluginRunner) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), consumerPluginRunnerKey, runner))
}

func RunConsumerPlugins(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if alreadyRun, _ := r.Context().Value(consumerPluginsRunKey).(bool); alreadyRun {
		next.ServeHTTP(w, r)
		return
	}
	runner, _ := r.Context().Value(consumerPluginRunnerKey).(ConsumerPluginRunner)
	if runner == nil {
		next.ServeHTTP(w, r)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), consumerPluginsRunKey, true))
	runner(w, r, next)
}

func WithConsumerPluginOverrides(r *http.Request, names map[string]struct{}) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), consumerOverridesKey, names))
}

func ConsumerPluginOverrides(r *http.Request, name string) bool {
	names, _ := r.Context().Value(consumerOverridesKey).(map[string]struct{})
	_, ok := names[name]
	return ok
}

func contextValue(c context.Context, key string) any {
	if value := c.Value(key); value != nil {
		return value
	}
	return c.Value(ContextKey(key))
}

// GetString returns the value associated with the key as a string.
func GetString(c context.Context, key string) (s string) {
	if val := contextValue(c, key); val != nil {
		s, _ = val.(string)
	}
	return
}

const (
	ApisixVarsKey   ContextKey = "apisix_vars"
	requestStateKey ContextKey = "request_state"
)

// RequestState owns the mutable per-request maps and typed hot fields behind a
// single context value. The maps remain available for plugin compatibility.
type RequestState struct {
	ApisixVars  map[string]any
	RequestVars map[string]any
	recycled    atomic.Bool

	Status       int
	BalancerIP   string
	BalancerPort string
	RouteID      string
	ServiceID    string
}

func GetRequestState(r *http.Request) *RequestState {
	state, _ := r.Context().Value(requestStateKey).(*RequestState)
	return state
}

func WithApisixVars(r *http.Request, vars map[string]string) *http.Request {
	state := GetRequestState(r)
	if state == nil {
		state = newRequestState()
		state.RequestVars, _ = r.Context().Value(RequestVarsKey).(map[string]any)
		r = r.WithContext(context.WithValue(r.Context(), requestStateKey, state))
	}
	if state.ApisixVars == nil {
		state.ApisixVars = newVars()
	}
	for k, v := range vars {
		state.ApisixVars[k] = v
	}
	return r
}

func GetApisixVars(r *http.Request) map[string]any {
	if state := GetRequestState(r); state != nil {
		return state.ApisixVars
	}
	vars, _ := r.Context().Value(ApisixVarsKey).(map[string]any)
	return vars
}

func GetApisixVar(r *http.Request, key string) any {
	vars := GetApisixVars(r)
	if val, ok := vars[key]; ok {
		return val
	}
	return ""
}

func RegisterApisixVar(r *http.Request, key string, val any) {
	vars := GetApisixVars(r)
	if vars == nil {
		return
	}
	vars[key] = val
	state := GetRequestState(r)
	if state == nil {
		return
	}
	switch key {
	case "$status":
		state.Status, _ = val.(int)
	case "$balancer_ip":
		state.BalancerIP, _ = val.(string)
	case "$balancer_port":
		state.BalancerPort, _ = val.(string)
	case "$route_id":
		state.RouteID, _ = val.(string)
	case "$service_id":
		state.ServiceID, _ = val.(string)
	}
}

func AttachConsumer(r *http.Request, consumer resource.Consumer) {
	RegisterApisixVar(r, "$consumer", consumer)
	RegisterApisixVar(r, "$consumer_name", consumer.Username)
	RegisterApisixVar(r, "$consumer_group_id", consumer.GroupID)
	r.Header.Set("X-Consumer-Username", consumer.Username)
	// reference: https://github.com/apache/apisix/blob/master/apisix/consumer.lua#L84C1-L89C4
}

func RecycleVars(r *http.Request) {
	if state := GetRequestState(r); state != nil {
		putRequestState(state)
		return
	}
	putBack(GetApisixVars(r))
	putBack(GetRequestVars(r))
}

const RequestVarsKey ContextKey = "request_vars"

func WithRequestVars(r *http.Request) *http.Request {
	state := GetRequestState(r)
	if state == nil {
		state = newRequestState()
		state.ApisixVars, _ = r.Context().Value(ApisixVarsKey).(map[string]any)
		r = r.WithContext(context.WithValue(r.Context(), requestStateKey, state))
	}
	if state.RequestVars == nil {
		state.RequestVars = newVars()
	}
	return r
}

func GetRequestVars(r *http.Request) map[string]any {
	if state := GetRequestState(r); state != nil {
		return state.RequestVars
	}
	vars, _ := r.Context().Value(RequestVarsKey).(map[string]any)
	return vars
}

func GetRequestVar(r *http.Request, key string) any {
	vars := GetRequestVars(r)
	if val, ok := vars[key]; ok {
		return val
	}
	return nil
}

func RegisterRequestVar(r *http.Request, key string, val any) {
	vars := GetRequestVars(r)
	vars[key] = val
}

const RequestBodyKey = "$request_body"

// ReadRequestBody returns the body in []byte without changing the origin
// body. The read inherits any bound already applied to r.Body, for example
// client-control's MaxBytesReader.
func ReadRequestBody(r *http.Request) ([]byte, error) {
	return readRequestBody(r, 0)
}

// ReadRequestBodyWithLimit returns the body in []byte bounded at max bytes.
// Oversized reads surface a *http.MaxBytesError detectable with errors.As.
func ReadRequestBodyWithLimit(r *http.Request, max int64) ([]byte, error) {
	return readRequestBody(r, max)
}

func readRequestBody(r *http.Request, max int64) ([]byte, error) {
	bodyInCtx := GetRequestVar(r, RequestBodyKey)
	if bodyInCtx != nil {
		body, ok := bodyInCtx.([]byte)
		if !ok {
			return nil, fmt.Errorf("$request_body context value has type %T, want []byte", bodyInCtx)
		}
		return body, nil
	}

	body, err := readBoundedBody(r, max)

	if r.Body != nil {
		if cerr := r.Body.Close(); cerr != nil && err == nil {
			logger.Errorf("request body close fail: %s", cerr)
			err = cerr
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(body))

	if GetRequestVars(r) != nil {
		RegisterRequestVar(r, RequestBodyKey, body)
	}
	return body, err
}

// bodyLimitResponseWriter satisfies http.MaxBytesReader's writer requirement;
// only the reader's typed error is needed here.
type bodyLimitResponseWriter struct{}

func (bodyLimitResponseWriter) Header() http.Header         { return http.Header{} }
func (bodyLimitResponseWriter) WriteHeader(int)             {}
func (bodyLimitResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func readBoundedBody(r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	source := r.Body
	if max > 0 {
		source = http.MaxBytesReader(bodyLimitResponseWriter{}, source, max)
	}
	return io.ReadAll(source)
}
