package limit_conn

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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
