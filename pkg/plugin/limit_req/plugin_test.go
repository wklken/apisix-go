package limit_req

import (
	"net/http"
	"net/http/httptest"
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
	if got := rejected.Body.String(); got != "slow down\n" {
		t.Fatalf("response body = %q, want %q", got, "slow down\n")
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
