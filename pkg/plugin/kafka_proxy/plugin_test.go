package kafka_proxy

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

func TestHandlerStoresSASLConfigForKafkaUpstream(t *testing.T) {
	p := newTestPlugin(t, Config{
		SASL: &SASL{
			Username: "user",
			Password: "pwd",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/kafka", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !SASLEnabled(r) {
			t.Fatal("SASLEnabled() = false, want true")
		}
		if got := SASLUsername(r); got != "user" {
			t.Fatalf("SASLUsername() = %q, want user", got)
		}
		if got := SASLPassword(r); got != "pwd" {
			t.Fatalf("SASLPassword() = %q, want pwd", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerDoesNotSetSASLContextWhenDisabled(t *testing.T) {
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "/kafka", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SASLEnabled(r) {
			t.Fatal("SASLEnabled() = true, want false")
		}
		if got := SASLUsername(r); got != "" {
			t.Fatalf("SASLUsername() = %q, want empty", got)
		}
		if got := SASLPassword(r); got != "" {
			t.Fatalf("SASLPassword() = %q, want empty", got)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
}
