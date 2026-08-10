package base

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	projectjson "github.com/wklken/apisix-go/pkg/json"
)

func TestCaptureAuthorizationFactsUsesTrustedRequestIdentity(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://gateway.example:8443/orders?role=admin&role=reader", nil)
	r.Host = "gateway.example:8443"
	r.RemoteAddr = "192.0.2.10:41000"
	r.Header.Add("X-Role", "admin")
	r.Header.Add("X-Role", "reader")
	r.Header.Set("X-Forwarded-Proto", "http")
	r = r.WithContext(context.WithValue(r.Context(), apisixctx.RemoteAddrKey, "198.51.100.20"))
	r = r.WithContext(context.WithValue(r.Context(), apisixctx.RemotePortKey, "8443"))
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, testNetAddr("127.0.0.1:9080")))

	facts := CaptureAuthorizationFacts(r, "127.0.0.1:9080", AuthorizationResource{
		ID: "route-1", Name: "orders", URI: "/orders/*",
	}, AuthorizationResource{ID: "service-1", Name: "orders-service"})

	if facts.Version != 1 {
		t.Fatalf("Version = %d, want 1", facts.Version)
	}
	if facts.Scheme != "https" {
		t.Fatalf("Scheme = %q, want https", facts.Scheme)
	}
	if facts.Method != http.MethodGet || facts.Host != "gateway.example:8443" {
		t.Fatalf("request identity = %#v, want GET gateway.example:8443", facts)
	}
	if facts.Path != "/orders" || facts.RawQuery != "role=admin&role=reader" {
		t.Fatalf("URL facts = %#v", facts)
	}
	if got := facts.Headers["X-Role"]; len(got) != 2 || got[0] != "admin" || got[1] != "reader" {
		t.Fatalf("X-Role = %v, want both values", got)
	}
	if got := facts.Headers["Host"]; len(got) != 1 || got[0] != "gateway.example:8443" {
		t.Fatalf("Host header fact = %v", got)
	}
	if facts.ClientIP != "198.51.100.20" || facts.ClientPort != "8443" {
		t.Fatalf("client identity = %s:%s", facts.ClientIP, facts.ClientPort)
	}
	if facts.ServerAddr != "127.0.0.1" || facts.ServerPort != "9080" {
		t.Fatalf("server identity = %s:%s", facts.ServerAddr, facts.ServerPort)
	}
	if facts.Route.ID != "route-1" || facts.Route.Name != "orders" || facts.Route.URI != "/orders/*" {
		t.Fatalf("route = %#v", facts.Route)
	}
	if facts.Service.ID != "service-1" || facts.Service.Name != "orders-service" {
		t.Fatalf("service = %#v", facts.Service)
	}
}

func TestCaptureAuthorizationFactsCopiesHeadersAndExposesOnlySafeResources(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/submit?x=1", nil)
	r.Host = "example.test"
	r.RemoteAddr = "203.0.113.10:41000"
	r.Header.Add("X-Role", "first")
	r.Header.Add("X-Role", "second")

	facts := CaptureAuthorizationFacts(r, "127.0.0.1", AuthorizationResource{
		ID: "route-safe", Name: "safe", URI: "/safe",
	}, AuthorizationResource{ID: "service-safe", Name: "service"})

	r.Header["X-Role"][0] = "mutated"
	if got := facts.Headers["X-Role"]; got[0] != "first" || got[1] != "second" {
		t.Fatalf("copied X-Role = %v, want original values", got)
	}

	body, err := projectjson.Marshal(facts)
	if err != nil {
		t.Fatalf("marshal facts: %v", err)
	}
	raw := string(body)
	for _, secret := range []string{"plugins", "upstream"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("serialized facts contain %q: %s", secret, raw)
		}
	}
}

func TestCaptureAuthorizationFactsFallsBackForClientAndServerPorts(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	r.RemoteAddr = "203.0.113.10:41000"

	facts := CaptureAuthorizationFacts(r, "[2001:db8::1]", AuthorizationResource{}, AuthorizationResource{})
	if facts.ClientIP != "203.0.113.10" || facts.ClientPort != "41000" {
		t.Fatalf("socket client identity = %s:%s", facts.ClientIP, facts.ClientPort)
	}
	if facts.ServerAddr != "[2001:db8::1]" || facts.ServerPort != "" {
		t.Fatalf("unsplit server identity = %s:%s", facts.ServerAddr, facts.ServerPort)
	}
}

type testNetAddr string

func (a testNetAddr) Network() string { return "tcp" }

func (a testNetAddr) String() string { return string(a) }

var _ net.Addr = testNetAddr("")
