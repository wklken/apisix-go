package ctx

import (
	"context"
	"net/http"
	"time"
)

// inspired by gin/context.go, but we use context.Context instead of gin.Context

// GetString returns the value associated with the key as a string.
func GetString(c context.Context, key string) (s string) {
	if val := c.Value(key); val != nil {
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

const ApisixVarsKey = "apisix_vars"

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

func RecycleVars(r *http.Request) {
	putBack(GetApisixVars(r))

	putBack(GetRequestVars(r))
}

const RequestVarsKey = "request_vars"

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
	return ""
}

func RegisterRequestVar(r *http.Request, key string, val any) {
	vars := GetRequestVars(r)
	vars[key] = val
}
