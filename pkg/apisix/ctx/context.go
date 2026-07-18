package ctx

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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
	consumerOverridesKey    ContextKey = "consumer_plugin_overrides"
	beforeProxyHooksKey     ContextKey = "before_proxy_hooks"
)

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

// GetInt returns the value associated with the key as a int.
func GetInt(c context.Context, key string) (i int) {
	if val := c.Value(key); val != nil {
		i, _ = val.(int)
	}
	return
}

// GetInt64 returns the value associated with the key as a int64.
func GetInt64(c context.Context, key string) (i int64) {
	if val := c.Value(key); val != nil {
		i, _ = val.(int64)
	}
	return
}

// GetBool returns the value associated with the key as a bool.
func GetBool(c context.Context, key string) (b bool) {
	if val := c.Value(key); val != nil {
		b, _ = val.(bool)
	}
	return
}

func GetBytes(c context.Context, key string) (b []byte) {
	val := c.Value(key)
	if val == nil {
		return nil
	}

	b, _ = val.([]byte)
	return b
}

// GetMapStringString returns the value associated with the key as a map[string]string.
func GetMapStringString(c context.Context, key string) (m map[string]string) {
	if val := c.Value(key); val != nil {
		m, _ = val.(map[string]string)
	}
	return
}

// GetMapStringAny returns the value associated with the key as a map[string]any.
func GetMapStringAny(c context.Context, key string) (m map[string]any) {
	if val := c.Value(key); val != nil {
		m, _ = val.(map[string]any)
	}
	return
}

// GetSliceString returns the value associated with the key as a []string.
func GetSliceString(c context.Context, key string) (s []string) {
	if val := c.Value(key); val != nil {
		s, _ = val.([]string)
	}
	return
}

// GetTime returns the value associated with the key as a time.Time.
func GetTime(c context.Context, key string) (t time.Time) {
	if val := c.Value(key); val != nil {
		t, _ = val.(time.Time)
	}
	return
}

// GetDuration returns the value associated with the key as a time.Duration.
func GetDuration(c context.Context, key string) (d time.Duration) {
	if val := c.Value(key); val != nil {
		d, _ = val.(time.Duration)
	}
	return
}

// func GetRequestBody(r *http.Request) []byte {
// 	body, _ := r.Context().Value(RequestBodyKey).([]byte)
// 	return body
// }

// func WithRequestBody(r *http.Request, body []byte) *http.Request {
// 	r = r.WithContext(context.WithValue(r.Context(), RequestBodyKey, body))
// 	return r
// }

const ApisixVarsKey ContextKey = "apisix_vars"

func WithApisixVars(r *http.Request, vars map[string]string) *http.Request {
	apisixVars := newVars()
	for k, v := range vars {
		apisixVars[k] = v
	}

	r = r.WithContext(context.WithValue(r.Context(), ApisixVarsKey, apisixVars))
	return r
}

func GetApisixVars(r *http.Request) map[string]any {
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
	vars[key] = val
}

func AttachConsumer(r *http.Request, consumer resource.Consumer) {
	RegisterApisixVar(r, "$consumer", consumer)
	RegisterApisixVar(r, "$consumer_name", consumer.Username)
	RegisterApisixVar(r, "$consumer_group_id", consumer.GroupID)
	r.Header.Set("X-Consumer-Username", consumer.Username)
	// reference: https://github.com/apache/apisix/blob/master/apisix/consumer.lua#L84C1-L89C4
}

func RecycleVars(r *http.Request) {
	putBack(GetApisixVars(r))

	putBack(GetRequestVars(r))
}

const RequestVarsKey ContextKey = "request_vars"

func WithRequestVars(r *http.Request) *http.Request {
	vars := newVars()
	r = r.WithContext(context.WithValue(r.Context(), RequestVarsKey, vars))
	return r
}

func GetRequestVars(r *http.Request) map[string]any {
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

// ReadRequestBody will return the body in []byte, without change the origin body
func ReadRequestBody(r *http.Request) ([]byte, error) {
	bodyInCtx := GetRequestVar(r, RequestBodyKey)
	if bodyInCtx != nil {
		return bodyInCtx.([]byte), nil
	}

	body, err := io.ReadAll(r.Body)

	if cerr := r.Body.Close(); cerr != nil {
		logger.Errorf("request body close fail: %s", cerr)
	}

	r.Body = io.NopCloser(bytes.NewReader(body))

	RegisterRequestVar(r, RequestBodyKey, body)
	return body, err
}
