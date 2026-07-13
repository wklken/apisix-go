package ip_restriction

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestWhitelistRejectsWithJSONMessage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Whitelist: []string{"10.0.0.1"},
	})
	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "192.168.1.1:12345"

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("ip-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"Your IP address is not allowed"}` {
		t.Fatalf("body = %q", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestBlacklistRejectsCustomMessage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Blacklist: []string{"192.168.1.0/24"},
		Message:   "blocked ip",
	})
	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "192.168.1.9:12345"

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("ip-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"blocked ip"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestBlacklistUsesConfiguredResponseCode(t *testing.T) {
	p := newTestPlugin(t, Config{
		Blacklist:    []string{"192.168.1.0/24"},
		ResponseCode: http.StatusNotFound,
	})
	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "192.168.1.9:12345"

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("ip-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestPostInitRejectsInvalidIPDefinition(t *testing.T) {
	p := &Plugin{config: Config{Whitelist: []string{"not-an-ip"}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid IP rejection")
	}
}

func TestSchemaValidatesResponseCodeBounds(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"whitelist":     []any{"127.0.0.1"},
		"response_code": http.StatusBadRequest,
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("ip-restriction schema should reject response_code below 403")
	}
}

func TestRemoteAddrContextOverridesRequestRemoteAddr(t *testing.T) {
	p := newTestPlugin(t, Config{
		Blacklist: []string{"203.0.113.8"},
	})
	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req = req.WithContext(context.WithValue(req.Context(), apisixctx.RemoteAddrKey, "203.0.113.8"))

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("ip-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestAllowedIPFallsThrough(t *testing.T) {
	p := newTestPlugin(t, Config{
		Whitelist: []string{"127.0.0.1"},
	})
	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	rr := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func newTestPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()

	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}
