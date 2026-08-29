package ip_restriction

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	if got := rr.Body.String(); got != "{\"message\":\"Your IP address is not allowed\"}\n" {
		t.Fatalf("body = %q", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX response type", got)
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

	if got := rr.Body.String(); got != "{\"message\":\"blocked ip\"}\n" {
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

func TestWhitelistSupportsIPv4AndIPv6Definitions(t *testing.T) {
	tests := []struct {
		name       string
		definition string
		remoteAddr string
	}{
		{
			name:       "IPv4 CIDR",
			definition: "192.0.2.9/24",
			remoteAddr: "192.0.2.10:12345",
		},
		{
			name:       "IPv6 address",
			definition: "2001:db8::9",
			remoteAddr: "[2001:db8::9]:12345",
		},
		{
			name:       "IPv6 CIDR",
			definition: "2001:db8::/32",
			remoteAddr: "[2001:db8:1::9]:12345",
		},
		{
			name:       "IPv4-mapped IPv6 address",
			definition: "192.0.2.9",
			remoteAddr: "[::ffff:192.0.2.9]:12345",
		},
		{
			name:       "IPv4 CIDR with IPv4-mapped IPv6 address",
			definition: "192.0.2.0/24",
			remoteAddr: "[::ffff:192.0.2.9]:12345",
		},
		{
			name:       "IPv4-mapped IPv6 CIDR with IPv4 address",
			definition: "::ffff:192.0.2.0/120",
			remoteAddr: "192.0.2.9:12345",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Whitelist: []string{test.definition}})
			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = test.remoteAddr

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
		})
	}
}

func TestBlacklistAllowsAddressOutsideDefinitions(t *testing.T) {
	p := newTestPlugin(t, Config{Blacklist: []string{"192.0.2.0/24"}})
	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "198.51.100.9:12345"

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

func TestIPMatcherDoesNotAllocateForExactAddress(t *testing.T) {
	p := newTestPlugin(t, Config{Whitelist: []string{"192.0.2.9"}})

	allocations := testing.AllocsPerRun(1000, func() {
		if !p.filter.Allowed("192.0.2.9") {
			t.Fatal("configured address should be allowed")
		}
	})
	if allocations != 0 {
		t.Fatalf("allocations per exact address match = %v, want 0", allocations)
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
