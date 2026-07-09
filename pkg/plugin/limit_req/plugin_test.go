package limit_req

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func performRequest(handler http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = remoteAddr

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestPostInitDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:  1,
		Burst: 1,
		Key:   "remote_addr",
	})

	if p.GetName() != "limit-req" {
		t.Fatalf("GetName() = %q, want limit-req", p.GetName())
	}
	if p.GetPriority() != 1001 {
		t.Fatalf("GetPriority() = %d, want 1001", p.GetPriority())
	}
	if p.config.Policy != "local" {
		t.Fatalf("Policy = %q, want local", p.config.Policy)
	}
	if p.config.KeyType != "var" {
		t.Fatalf("KeyType = %q, want var", p.config.KeyType)
	}
	if p.config.RejectedCode != http.StatusServiceUnavailable {
		t.Fatalf("RejectedCode = %d, want %d", p.config.RejectedCode, http.StatusServiceUnavailable)
	}
	if p.config.Nodelay == nil || *p.config.Nodelay {
		t.Fatalf("Nodelay = %v, want false", p.config.Nodelay)
	}
}

func TestPostInitAcceptsRedisPolicyDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:      1,
		Burst:     1,
		Key:       "remote_addr",
		Policy:    "redis",
		RedisHost: "127.0.0.1",
	})

	if p.config.Policy != "redis" {
		t.Fatalf("Policy = %q, want redis", p.config.Policy)
	}
	if p.config.RedisPort != 6379 {
		t.Fatalf("RedisPort = %d, want 6379", p.config.RedisPort)
	}
	if p.config.RedisTimeout != 1000 {
		t.Fatalf("RedisTimeout = %d, want 1000", p.config.RedisTimeout)
	}
	if p.config.RedisSSL == nil || *p.config.RedisSSL {
		t.Fatalf("RedisSSL = %v, want false", p.config.RedisSSL)
	}
	if p.config.RedisSSLVerify == nil || *p.config.RedisSSLVerify {
		t.Fatalf("RedisSSLVerify = %v, want false", p.config.RedisSSLVerify)
	}
}

func TestSchemaAcceptsRedisPolicyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"rate":                    1,
		"burst":                   1,
		"key":                     "remote_addr",
		"policy":                  "redis",
		"redis_host":              "127.0.0.1",
		"redis_port":              6379,
		"redis_username":          "default",
		"redis_password":          "",
		"redis_database":          0,
		"redis_timeout":           1000,
		"redis_ssl":               false,
		"redis_ssl_verify":        false,
		"redis_keepalive_timeout": 10000,
		"redis_keepalive_pool":    100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected redis policy fields: %v", err)
	}
}

func TestHandlerUsesRedisLimiter(t *testing.T) {
	redisLimiter := &fakeRedisLimiter{allowed: true}
	p := newTestPlugin(t, Config{
		Rate:      1,
		Burst:     0,
		Key:       "remote_addr",
		Policy:    "redis",
		RedisHost: "127.0.0.1",
		Nodelay:   boolPtr(true),
	})
	p.redisLimiter = redisLimiter

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	res := performRequest(handler, "192.0.2.40:12345")
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
	if redisLimiter.key != "192.0.2.40" {
		t.Fatalf("redis key = %q, want 192.0.2.40", redisLimiter.key)
	}
	if redisLimiter.rate != 1 {
		t.Fatalf("redis rate = %f, want 1", redisLimiter.rate)
	}
	if redisLimiter.burst != 0 {
		t.Fatalf("redis burst = %f, want 0", redisLimiter.burst)
	}
}

func TestHandlerRejectsWhenRedisLimiterRejects(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:        1,
		Burst:       0,
		Key:         "remote_addr",
		Policy:      "redis",
		RedisHost:   "127.0.0.1",
		RejectedMsg: "slow down",
		Nodelay:     boolPtr(true),
	})
	p.redisLimiter = &fakeRedisLimiter{allowed: false}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	res := performRequest(handler, "192.0.2.50:12345")
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if got := res.Body.String(); got != `{"error_msg":"slow down"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"slow down"}`)
	}
}

func TestHandlerRejectsRequestsAboveRateAndBurst(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:    1,
		Burst:   0,
		Key:     "remote_addr",
		Nodelay: boolPtr(true),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := performRequest(handler, "192.0.2.10:12345")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusNoContent)
	}

	second := performRequest(handler, "192.0.2.10:23456")
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second response code = %d, want %d", second.Code, http.StatusServiceUnavailable)
	}
}

func TestHandlerUsesRejectedMessage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:        1,
		Burst:       0,
		Key:         "remote_addr",
		RejectedMsg: "slow down",
		Nodelay:     boolPtr(true),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	performRequest(handler, "192.0.2.20:12345")
	rejected := performRequest(handler, "192.0.2.20:23456")

	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", rejected.Code, http.StatusServiceUnavailable)
	}
	if got := rejected.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := rejected.Body.String(); got != `{"error_msg":"slow down"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"slow down"}`)
	}
}

func TestHandlerTracksSeparateKeys(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:    1,
		Burst:   0,
		Key:     "remote_addr",
		Nodelay: boolPtr(true),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	performRequest(handler, "192.0.2.30:12345")

	secondKey := performRequest(handler, "192.0.2.31:12345")
	if secondKey.Code != http.StatusNoContent {
		t.Fatalf("second key response code = %d, want %d", secondKey.Code, http.StatusNoContent)
	}
}

func boolPtr(v bool) *bool {
	return &v
}

type fakeRedisLimiter struct {
	key     string
	rate    float64
	burst   float64
	delay   time.Duration
	allowed bool
	err     error
}

func (f *fakeRedisLimiter) incoming(key string, rate float64, burst float64) (time.Duration, bool, error) {
	f.key = key
	f.rate = rate
	f.burst = burst
	return f.delay, f.allowed, f.err
}
