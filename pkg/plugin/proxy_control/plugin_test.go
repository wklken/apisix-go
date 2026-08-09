package proxy_control

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestPostInitDefaultsRequestBufferingToTrue(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.RequestBuffering == nil || !*p.config.RequestBuffering {
		t.Fatalf("request_buffering = %v, want true by default", p.config.RequestBuffering)
	}
}

func TestHandlerSetsRequestBufferingContext(t *testing.T) {
	requestBuffering := false
	p := newTestPlugin(t, Config{RequestBuffering: &requestBuffering})

	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("body"))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := GetRequestBuffering(r); got {
			t.Fatalf("GetRequestBuffering() = %v, want false", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestRequestBufferingStateCarriesFixedLimit(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("body"))
	if got := GetRequestBufferingLimit(request); got != 0 {
		t.Fatalf("GetRequestBufferingLimit() without state = %d, want 0", got)
	}

	request = WithRequestBuffering(request, true)
	if !GetRequestBuffering(request) {
		t.Fatal("GetRequestBuffering() = false after enabling buffering")
	}
	if got := GetRequestBufferingLimit(request); got != DefaultRequestBufferingLimit {
		t.Fatalf("GetRequestBufferingLimit() = %d, want %d", got, DefaultRequestBufferingLimit)
	}

	request = WithRequestBuffering(request, false)
	if GetRequestBuffering(request) {
		t.Fatal("GetRequestBuffering() = true after disabling buffering")
	}
	if got := GetRequestBufferingLimit(request); got != DefaultRequestBufferingLimit {
		t.Fatalf("disabled GetRequestBufferingLimit() = %d, want %d", got, DefaultRequestBufferingLimit)
	}
}
