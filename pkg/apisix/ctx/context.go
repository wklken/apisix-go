package ctx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/resource"
)

type BeforeProxyHook func(*http.Request) error

type BeforeProxyHookRegistration struct {
	Owner string
	Phase string
	Hook  BeforeProxyHook
}

type beforeProxyHooks struct {
	once          sync.Once
	registrations []BeforeProxyHookRegistration
	err           error
	panicValue    any
	panicked      bool
}

// inspired by gin/context.go, but we use context.Context instead of gin.Context

type ContextKey string

const (
	ProxyRewriteKey        ContextKey = "proxy-rewrite"
	RequestIDKey           ContextKey = "request_id"
	RemoteAddrKey          ContextKey = "remote_addr"
	RemotePortKey          ContextKey = "remote_port"
	authProbeDiagnosticKey ContextKey = "auth_probe_diagnostic_recorder"
	consumerOverridesKey   ContextKey = "consumer_plugin_overrides"
	beforeProxyHooksKey    ContextKey = "before_proxy_hooks"
	trustedProxyKey        ContextKey = "trusted_proxy"
)

type (
	authenticationStateKey     struct{}
	requestHeaderProvenanceKey struct{}
	forwardedForCandidateKey   struct{}
	matchedRouteKey            struct{}
)

type matchedRoute struct {
	uri  string
	host string
}

// WithMatchedRoute records the immutable route pattern and host pattern
// selected for one request. Compiled route metadata remains shared and is
// never mutated while requests are matched.
func WithMatchedRoute(r *http.Request, uri string, host string) *http.Request {
	if r == nil {
		return nil
	}
	return r.WithContext(context.WithValue(r.Context(), matchedRouteKey{}, matchedRoute{
		uri: uri, host: host,
	}))
}

// MatchedRoute returns the route pattern and host pattern selected for the
// current request.
func MatchedRoute(r *http.Request) (uri string, host string, ok bool) {
	if r == nil {
		return "", "", false
	}
	matched, ok := r.Context().Value(matchedRouteKey{}).(matchedRoute)
	return matched.uri, matched.host, ok
}

// WithForwardedForCandidate preserves ingress-supplied X-Forwarded-For values
// after the public header has been removed. The values remain untrusted; only
// real-ip may consume them after independently validating the socket peer.
func WithForwardedForCandidate(r *http.Request, values []string) *http.Request {
	if r == nil || len(values) == 0 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), forwardedForCandidateKey{}, slices.Clone(values)))
}

// ForwardedForCandidate returns a detached copy of the ingress-supplied
// X-Forwarded-For values retained for real-ip peer validation.
func ForwardedForCandidate(r *http.Request) []string {
	if r == nil {
		return nil
	}
	values, _ := r.Context().Value(forwardedForCandidateKey{}).([]string)
	return slices.Clone(values)
}

type requestHeaderProvenance struct {
	trusted    http.Header
	overridden map[string]struct{}
}

// WithRequestHeaderProvenance records which headers were supplied by an
// internal request overlay and preserves the corresponding outer values for
// authentication plugins.
func WithRequestHeaderProvenance(r *http.Request, trusted http.Header, overridden []string) *http.Request {
	if r == nil || len(overridden) == 0 {
		return r
	}
	keys := make(map[string]struct{}, len(overridden))
	for _, key := range overridden {
		keys[http.CanonicalHeaderKey(key)] = struct{}{}
	}
	provenance := requestHeaderProvenance{trusted: trusted.Clone(), overridden: keys}
	return r.WithContext(context.WithValue(r.Context(), requestHeaderProvenanceKey{}, provenance))
}

// RestoreTrustedRequestHeader replaces an internal overlay with the outer
// header before authentication and before the request can reach an upstream.
func RestoreTrustedRequestHeader(r *http.Request, name string) string {
	if r == nil {
		return ""
	}
	provenance, ok := r.Context().Value(requestHeaderProvenanceKey{}).(requestHeaderProvenance)
	if !ok {
		return r.Header.Get(name)
	}
	canonicalName := http.CanonicalHeaderKey(name)
	if _, overridden := provenance.overridden[canonicalName]; !overridden {
		return r.Header.Get(name)
	}
	r.Header.Del(name)
	for _, value := range provenance.trusted.Values(name) {
		r.Header.Add(name, value)
	}
	return r.Header.Get(name)
}

// TrustedRequestHeaders returns a clone with internal overlay values replaced
// by their authenticated outer values. It keeps nested internal requests from
// promoting a prior overlay into a trusted source.
func TrustedRequestHeaders(r *http.Request) http.Header {
	if r == nil {
		return nil
	}
	headers := r.Header.Clone()
	provenance, ok := r.Context().Value(requestHeaderProvenanceKey{}).(requestHeaderProvenance)
	if !ok {
		return headers
	}
	for key := range provenance.overridden {
		headers.Del(key)
		for _, value := range provenance.trusted.Values(key) {
			headers.Add(key, value)
		}
	}
	return headers
}

// AuthenticationState is the clone-safe result published by an explicit
// consumer authenticator. Source is the exact factory key that authenticated
// the request; it is intentionally not derived from Consumer().
type AuthenticationState struct {
	Source   string
	consumer resource.Consumer
}

func NewAuthenticationState(source string, consumer resource.Consumer) AuthenticationState {
	return AuthenticationState{Source: source, consumer: cloneConsumer(consumer)}
}

func (s AuthenticationState) Consumer() resource.Consumer {
	return cloneConsumer(s.consumer)
}

func WithAuthenticationState(r *http.Request, state AuthenticationState) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authenticationStateKey{}, state))
}

func AuthenticationStateFrom(r *http.Request) (AuthenticationState, bool) {
	if r == nil {
		return AuthenticationState{}, false
	}
	switch state := r.Context().Value(authenticationStateKey{}).(type) {
	case AuthenticationState:
		return state, true
	case *AuthenticationState:
		if state == nil {
			return AuthenticationState{}, false
		}
		return *state, true
	default:
		return AuthenticationState{}, false
	}
}

// NewAuthenticationProbeRequest makes an isolated request for a losing auth
// probe. Headers, URL state, body readers, diagnostics, and authentication
// state are independent from the parent request.
func NewAuthenticationProbeRequest(r *http.Request) *http.Request {
	if r == nil {
		return nil
	}
	probeContext := context.WithValue(r.Context(), authenticationStateKey{}, nil)
	probeContext = context.WithValue(probeContext, authProbeDiagnosticKey, nil)
	probe := r.Clone(probeContext)
	// Body replay is deliberately not owned by this generic helper. In
	// particular, multi-auth must apply each child's bounded BodyIsolation
	// policy before installing an independent probe reader. Reading here would
	// consume or buffer an unbounded request body before that limit applies.
	probe.Body = http.NoBody
	probe.GetBody = nil
	return probe
}

func cloneConsumer(consumer resource.Consumer) resource.Consumer {
	consumer.Plugins = cloneConsumerMap(consumer.Plugins)
	if consumer.Labels != nil {
		consumer.Labels = cloneConsumerAnyMap(consumer.Labels)
	}
	return consumer
}

func cloneConsumerMap(source map[string]resource.PluginConfig) map[string]resource.PluginConfig {
	if source == nil {
		return nil
	}
	cloned := make(map[string]resource.PluginConfig, len(source))
	for key, value := range source {
		cloned[key] = cloneConsumerValue(value)
	}
	return cloned
}

func cloneConsumerAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneConsumerValue(value)
	}
	return cloned
}

func cloneConsumerValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneConsumerReflect(reflect.ValueOf(value)).Interface()
}

func cloneConsumerReflect(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneConsumerReflect(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			result.SetMapIndex(cloneConsumerReflect(iter.Key()), cloneConsumerReflect(iter.Value()))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneConsumerReflect(value.Index(i)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneConsumerReflect(value.Index(i)))
		}
		return result
	default:
		return value
	}
}

func WithTrustedProxy(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), trustedProxyKey, true))
}

func IsTrustedProxy(r *http.Request) bool {
	trusted, _ := r.Context().Value(trustedProxyKey).(bool)
	return trusted
}

func WithBeforeProxyHook(r *http.Request, hook BeforeProxyHook) *http.Request {
	return WithBeforeProxyHookRegistration(r, BeforeProxyHookRegistration{Hook: hook})
}

func WithBeforeProxyHookRegistration(
	r *http.Request,
	registration BeforeProxyHookRegistration,
) *http.Request {
	registered, _ := r.Context().Value(beforeProxyHooksKey).(*beforeProxyHooks)
	registrations := make([]BeforeProxyHookRegistration, 0, 1)
	if registered != nil {
		registrations = append(registrations, registered.registrations...)
	}
	registrations = append(registrations, registration)
	return r.WithContext(context.WithValue(
		r.Context(),
		beforeProxyHooksKey,
		&beforeProxyHooks{registrations: registrations},
	))
}

func RunBeforeProxyHooks(r *http.Request) error {
	return RunBeforeProxyHookRegistrations(r, func(registration BeforeProxyHookRegistration) error {
		return registration.Hook(r)
	})
}

func RunBeforeProxyHookRegistrations(
	r *http.Request,
	invoke func(BeforeProxyHookRegistration) error,
) error {
	registered, _ := r.Context().Value(beforeProxyHooksKey).(*beforeProxyHooks)
	if registered == nil {
		return nil
	}
	registered.once.Do(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				registered.panicValue = recovered
				registered.panicked = true
			}
		}()
		for _, registration := range registered.registrations {
			if err := invoke(registration); err != nil {
				registered.err = err
				return
			}
		}
	})
	if registered.panicked {
		panic(registered.panicValue)
	}
	return registered.err
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
	// sensitiveQueryNames is request-local metadata used only at the logging
	// boundary. It is deliberately not exposed through the APISIX variable
	// maps, because those maps are also consumed by upstream-facing code.
	sensitiveQueryNames map[string]struct{}

	Status       int
	BalancerIP   string
	BalancerPort string
	RouteID      string
	ServiceID    string

	// RequestBodyRead caches the first bounded body read so repeated reads
	// return the same bytes and error instead of masking a size violation.
	RequestBodyRead bool
	RequestBody     []byte
	RequestBodyErr  error
}

func GetRequestState(r *http.Request) *RequestState {
	if r == nil {
		return nil
	}
	state, _ := r.Context().Value(requestStateKey).(*RequestState)
	return state
}

// RegisterSensitiveQueryName marks one query parameter as a credential for
// request logging. It never changes the request URL or query bytes.
func RegisterSensitiveQueryName(r *http.Request, name string) {
	if r == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if state := GetRequestState(r); state != nil {
		if state.sensitiveQueryNames == nil {
			state.sensitiveQueryNames = make(map[string]struct{})
		}
		state.sensitiveQueryNames[name] = struct{}{}
		return
	}
	// Requests may not have gone through the request lifecycle middleware yet.
	// Install the same state in place so the
	// registration follows later request helpers without a process-global
	// pointer registry or a lifetime leak.
	state := newRequestState()
	state.ApisixVars, _ = r.Context().Value(ApisixVarsKey).(map[string]any)
	state.RequestVars, _ = r.Context().Value(RequestVarsKey).(map[string]any)
	if state.ApisixVars == nil {
		state.ApisixVars = newVars()
	}
	if state.RequestVars == nil {
		state.RequestVars = newVars()
	}
	state.sensitiveQueryNames = map[string]struct{}{name: {}}
	*r = *r.WithContext(context.WithValue(r.Context(), requestStateKey, state))
}

// SensitiveQueryNames returns a detached set of query names registered for
// request logging. The returned map may be modified by the caller.
func SensitiveQueryNames(r *http.Request) map[string]struct{} {
	if r == nil {
		return nil
	}
	if state := GetRequestState(r); state != nil {
		if len(state.sensitiveQueryNames) == 0 {
			return nil
		}
		result := make(map[string]struct{}, len(state.sensitiveQueryNames))
		for name := range state.sensitiveQueryNames {
			result[name] = struct{}{}
		}
		return result
	}
	return nil
}

// IsSensitiveQueryName reports whether a query key is registered for logging
// redaction.
func IsSensitiveQueryName(r *http.Request, name string) bool {
	_, ok := SensitiveQueryNames(r)[name]
	return ok
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
	RegisterApisixVar(r, "$consumer", redactedConsumerView(consumer))
	RegisterApisixVar(r, "$consumer_name", consumer.Username)
	RegisterApisixVar(r, "$consumer_group_id", consumer.GroupID)
	r.Header.Set("X-Consumer-Username", consumer.Username)
	// reference: https://github.com/apache/apisix/blob/master/apisix/consumer.lua#L84C1-L89C4
}

func AttachConsumerFromAuthenticationState(r *http.Request) {
	state, ok := AuthenticationStateFrom(r)
	if !ok {
		return
	}
	AttachConsumer(r, state.Consumer())
}

func redactedConsumerView(consumer resource.Consumer) resource.Consumer {
	var plugins map[string]resource.PluginConfig
	if consumer.Plugins != nil {
		plugins = make(map[string]resource.PluginConfig, len(consumer.Plugins))
		for name := range consumer.Plugins {
			plugins[name] = nil
		}
	}
	var labels map[string]any
	if consumer.Labels != nil {
		labels = make(map[string]any, len(consumer.Labels))
		maps.Copy(labels, consumer.Labels)
	}
	return resource.Consumer{
		Username: consumer.Username,
		GroupID:  consumer.GroupID,
		Plugins:  plugins,
		Labels:   labels,
	}
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
	if vars == nil {
		return
	}
	vars[key] = val
	if key == RequestBodyKey {
		if body, ok := val.([]byte); ok {
			if state := GetRequestState(r); state != nil {
				state.RequestBodyRead = true
				state.RequestBody = body
				state.RequestBodyErr = nil
			}
		}
	}
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
	state := GetRequestState(r)
	if state == nil {
		*r = *WithRequestVars(r)
		state = GetRequestState(r)
	}
	if state.RequestBodyRead {
		return state.RequestBody, state.RequestBodyErr
	}

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
	if state := GetRequestState(r); state != nil {
		state.RequestBody = body
		state.RequestBodyErr = err
		state.RequestBodyRead = true
	}

	if err == nil && GetRequestVars(r) != nil {
		RegisterRequestVar(r, RequestBodyKey, body)
	}
	state.RequestBodyRead = true
	state.RequestBody = body
	state.RequestBodyErr = err
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
