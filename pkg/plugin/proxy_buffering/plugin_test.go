package proxy_buffering

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

func TestPostInitDefaultsDisableProxyBufferingToFalse(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.DisableProxyBuffering {
		t.Fatal("disable_proxy_buffering = true, want false by default")
	}
}

func TestHandlerSetsDisableProxyBufferingContext(t *testing.T) {
	p := newTestPlugin(t, Config{DisableProxyBuffering: true})

	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := GetDisableProxyBuffering(r); !got {
			t.Fatalf("GetDisableProxyBuffering() = %v, want true", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerFlushesResponseWhenBufferingDisabled(t *testing.T) {
	p := newTestPlugin(t, Config{DisableProxyBuffering: true})
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("chunk")); err != nil {
			t.Fatalf("write response: %v", err)
		}
	})).ServeHTTP(rr, req)

	if !rr.Flushed {
		t.Fatal("response writer was not flushed with proxy buffering disabled")
	}
	if rr.Body.String() != "chunk" {
		t.Fatalf("response body = %q, want chunk", rr.Body.String())
	}
}
