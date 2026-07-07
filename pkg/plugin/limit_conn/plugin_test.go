package limit_conn

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
	if got := rejected.Body.String(); got != "too many connections\n" {
		t.Fatalf("response body = %q, want %q", got, "too many connections\n")
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

func performRequest(handler http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = remoteAddr

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
