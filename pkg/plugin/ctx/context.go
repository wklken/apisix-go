package ctx

import (
	"context"
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