package real_ip

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestXForwardedForUsesLastAddress(t *testing.T) {
	p := newTestPlugin(t, Config{Source: "http_x_forwarded_for"})
	req := httptest.NewRequest(http.MethodGet, "/real-ip", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.9:9443")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := apisixctx.GetString(r.Context(), "remote_addr"); got != "203.0.113.9" {
			t.Fatalf("remote_addr = %q, want 203.0.113.9", got)
		}
		if got := apisixctx.GetString(r.Context(), "remote_port"); got != "9443" {
			t.Fatalf("remote_port = %q, want 9443", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestQueryArgSourceSetsBareIP(t *testing.T) {
	p := newTestPlugin(t, Config{Source: "arg_realip"})
	req := httptest.NewRequest(http.MethodGet, "/real-ip?realip=203.0.113.10", nil)

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := apisixctx.GetString(r.Context(), "remote_addr"); got != "203.0.113.10" {
			t.Fatalf("remote_addr = %q, want 203.0.113.10", got)
		}
		if got := apisixctx.GetString(r.Context(), "remote_port"); got != "" {
			t.Fatalf("remote_port = %q, want empty", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestTrustedAddressesSkipUntrustedRemote(t *testing.T) {
	p := newTestPlugin(t, Config{
		Source:           "http_x_forwarded_for",
		TrustedAddresses: []string{"127.0.0.0/24"},
	})
	req := httptest.NewRequest(http.MethodGet, "/real-ip", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.11")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := apisixctx.GetString(r.Context(), "remote_addr"); got != "" {
			t.Fatalf("remote_addr = %q, want empty", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestTrustedAddressesAllowTrustedRemote(t *testing.T) {
	p := newTestPlugin(t, Config{
		Source:           "http_x_forwarded_for",
		TrustedAddresses: []string{"127.0.0.0/24"},
	})
	req := httptest.NewRequest(http.MethodGet, "/real-ip", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.12")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := apisixctx.GetString(r.Context(), "remote_addr"); got != "203.0.113.12" {
			t.Fatalf("remote_addr = %q, want 203.0.113.12", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestRecursiveXForwardedForUsesLastNonTrustedAddress(t *testing.T) {
	recursive := true
	p := newTestPlugin(t, Config{
		Source:           "http_x_forwarded_for",
		TrustedAddresses: []string{"127.0.0.0/24"},
		Recursive:        &recursive,
	})
	req := httptest.NewRequest(http.MethodGet, "/real-ip", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 127.0.0.2")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := apisixctx.GetString(r.Context(), "remote_addr"); got != "198.51.100.9" {
			t.Fatalf("remote_addr = %q, want 198.51.100.9", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

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
