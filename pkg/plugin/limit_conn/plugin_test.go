package limit_conn

import (
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestPostInitDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
	})

	if p.GetName() != "limit-conn" {
		t.Fatalf("GetName() = %q, want limit-conn", p.GetName())
	}
	if p.GetPriority() != 1003 {
		t.Fatalf("GetPriority() = %d, want 1003", p.GetPriority())
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
}

func TestPostInitAcceptsRedisPolicyDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
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
	if p.config.RedisKeyTTL != 3600 {
		t.Fatalf("RedisKeyTTL = %d, want 3600", p.config.RedisKeyTTL)
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
		"conn":                    1,
		"burst":                   0,
		"default_conn_delay":      0.1,
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
		"key_ttl":                 3600,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected redis policy fields: %v", err)
	}
}

func TestHandlerRejectsConcurrentRequestsAboveConnAndBurst(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
	})

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		performRequest(handler, "192.0.2.10:12345")
	}()
	<-started

	second := performRequest(handler, "192.0.2.10:23456")
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second response code = %d, want %d", second.Code, http.StatusServiceUnavailable)
	}

	close(block)
	wg.Wait()

	afterRelease := performRequest(handler, "192.0.2.10:34567")
	if afterRelease.Code != http.StatusNoContent {
		t.Fatalf("after release response code = %d, want %d", afterRelease.Code, http.StatusNoContent)
	}
}

func TestHandlerUsesRejectedMessage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		RejectedMsg:      "too many connections",
	})

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		performRequest(handler, "192.0.2.20:12345")
	}()
	<-started

	rejected := performRequest(handler, "192.0.2.20:23456")
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", rejected.Code, http.StatusServiceUnavailable)
	}
	if got := rejected.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := rejected.Body.String(); got != `{"error_msg":"too many connections"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"too many connections"}`)
	}

	close(block)
	wg.Wait()
}

func TestHandlerUsesRedisLimiter(t *testing.T) {
	redisLimiter := &fakeRedisConnLimiter{allowed: true}
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
	})
	p.redisLimiter = redisLimiter

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), "192.0.2.70:12345")

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
	if redisLimiter.key != "192.0.2.70" {
		t.Fatalf("redis key = %q, want 192.0.2.70", redisLimiter.key)
	}
	if redisLimiter.left != 1 {
		t.Fatalf("redis leaving calls = %d, want 1", redisLimiter.left)
	}
}

func TestHandlerRejectsWhenRedisLimiterRejects(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
		RejectedMsg:      "too many connections",
	})
	p.redisLimiter = &fakeRedisConnLimiter{allowed: false}

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})), "192.0.2.80:12345")

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if got := res.Body.String(); got != `{"error_msg":"too many connections"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"too many connections"}`)
	}
}

func TestHandlerTracksSeparateKeys(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
	})

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RemoteAddr == "192.0.2.30:12345" {
			startedOnce.Do(func() {
				close(started)
			})
			<-block
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		performRequest(handler, "192.0.2.30:12345")
	}()
	<-started

	secondKey := performRequest(handler, "192.0.2.31:12345")
	if secondKey.Code != http.StatusNoContent {
		t.Fatalf("second key response code = %d, want %d", secondKey.Code, http.StatusNoContent)
	}

	close(block)
	wg.Wait()
}

func TestIncreaseUsesDefaultDelayWhenConfigured(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:                1,
		Burst:               2,
		DefaultConnDelay:    0.2,
		Key:                 "remote_addr",
		OnlyUseDefaultDelay: true,
	})

	firstDelay, allowed, err := p.increase("client", 1, 2)
	if err != nil {
		t.Fatalf("increase() error = %v", err)
	}
	if !allowed {
		t.Fatal("first request rejected, want allowed")
	}
	if firstDelay != 0 {
		t.Fatalf("first delay = %s, want 0", firstDelay)
	}

	secondDelay, allowed, err := p.increase("client", 1, 2)
	if err != nil {
		t.Fatalf("increase() error = %v", err)
	}
	if !allowed {
		t.Fatal("second request rejected, want allowed")
	}
	if secondDelay != 200*time.Millisecond {
		t.Fatalf("second delay = %s, want 200ms", secondDelay)
	}

	thirdDelay, allowed, err := p.increase("client", 1, 2)
	if err != nil {
		t.Fatalf("increase() error = %v", err)
	}
	if !allowed {
		t.Fatal("third request rejected, want allowed")
	}
	if thirdDelay != 200*time.Millisecond {
		t.Fatalf("third delay = %s, want 200ms", thirdDelay)
	}
}

func TestHandlerAppliesResolvedRules(t *testing.T) {
	p := newTestPlugin(t, Config{
		DefaultConnDelay: 0.1,
		RejectedCode:     http.StatusTooManyRequests,
		Rules: []Rule{
			{Conn: 2, Burst: 0, Key: "$http_x_tenant"},
			{Conn: 1, Burst: 0, Key: "$http_x_user"},
		},
	})

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-User") == "alice" {
			startedOnce.Do(func() {
				close(started)
			})
			<-block
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		performRequestWithHeaders(handler, "192.0.2.40:12345", map[string]string{
			"X-Tenant": "t1",
			"X-User":   "alice",
		})
	}()
	<-started

	rejected := performRequestWithHeaders(handler, "192.0.2.40:23456", map[string]string{
		"X-Tenant": "t1",
		"X-User":   "alice",
	})
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected response code = %d, want %d", rejected.Code, http.StatusTooManyRequests)
	}

	differentUser := performRequestWithHeaders(handler, "192.0.2.40:34567", map[string]string{
		"X-Tenant": "t1",
		"X-User":   "bob",
	})
	if differentUser.Code != http.StatusNoContent {
		t.Fatalf("different user response code = %d, want %d", differentUser.Code, http.StatusNoContent)
	}

	close(block)
	wg.Wait()

	afterRelease := performRequestWithHeaders(handler, "192.0.2.40:45678", map[string]string{
		"X-Tenant": "t1",
		"X-User":   "alice",
	})
	if afterRelease.Code != http.StatusNoContent {
		t.Fatalf("after release response code = %d, want %d", afterRelease.Code, http.StatusNoContent)
	}
}

func TestHandlerReturnsInternalServerErrorWhenAllRulesAreUnresolved(t *testing.T) {
	p := newTestPlugin(t, Config{
		DefaultConnDelay: 0.1,
		Rules: []Rule{
			{Conn: 1, Burst: 0, Key: "tenant"},
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := performRequest(handler, "192.0.2.50:12345")
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusInternalServerError)
	}
}

func TestHandlerAllowsDegradationWhenAllRulesAreUnresolved(t *testing.T) {
	allowDegradation := true
	p := newTestPlugin(t, Config{
		DefaultConnDelay: 0.1,
		AllowDegradation: &allowDegradation,
		Rules: []Rule{
			{Conn: 1, Burst: 0, Key: "tenant"},
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := performRequest(handler, "192.0.2.60:12345")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusNoContent)
	}
}

func TestHandlerResolvesStringConnAndBurst(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"conn":               "$http_x_conn",
		"burst":              "$http_x_burst",
		"default_conn_delay": 0.1,
		"key":                "remote_addr",
		"rejected_code":      http.StatusTooManyRequests,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("string conn/burst config should validate: %v", err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		performRequestWithHeaders(handler, "192.0.2.70:12345", map[string]string{
			"X-Conn":  "1",
			"X-Burst": "0",
		})
	}()
	<-started

	rejected := performRequestWithHeaders(handler, "192.0.2.70:23456", map[string]string{
		"X-Conn":  "1",
		"X-Burst": "0",
	})
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected response code = %d, want %d", rejected.Code, http.StatusTooManyRequests)
	}

	close(block)
	wg.Wait()
}

func TestHandlerResolvesStringRuleConnAndBurst(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"default_conn_delay": 0.1,
		"rejected_code":      http.StatusTooManyRequests,
		"rules": []any{
			map[string]any{
				"conn":  "$http_x_conn",
				"burst": "$http_x_burst",
				"key":   "$http_x_user",
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("string rule conn/burst config should validate: %v", err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		performRequestWithHeaders(handler, "192.0.2.80:12345", map[string]string{
			"X-Conn":  "1",
			"X-Burst": "0",
			"X-User":  "alice",
		})
	}()
	<-started

	rejected := performRequestWithHeaders(handler, "192.0.2.80:23456", map[string]string{
		"X-Conn":  "1",
		"X-Burst": "0",
		"X-User":  "alice",
	})
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected response code = %d, want %d", rejected.Code, http.StatusTooManyRequests)
	}

	close(block)
	wg.Wait()
}

func performRequest(handler http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = remoteAddr

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func performRequestWithHeaders(
	handler http.Handler,
	remoteAddr string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = remoteAddr
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

type fakeRedisConnLimiter struct {
	key     string
	delay   time.Duration
	allowed bool
	err     error
	left    int
}

func (f *fakeRedisConnLimiter) incoming(key string, conn int, burst int) (time.Duration, bool, error) {
	f.key = key
	return f.delay, f.allowed, f.err
}

func (f *fakeRedisConnLimiter) leaving(key string) error {
	f.left++
	return f.err
}
