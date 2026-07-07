package ai

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()

	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerPassesThrough(t *testing.T) {
	p := newTestPlugin(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/v1/chat/completions", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-AI-Test", "next")
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	if rr.Header().Get("X-AI-Test") != "next" {
		t.Fatalf("pass-through header = %q, want next", rr.Header().Get("X-AI-Test"))
	}
}
