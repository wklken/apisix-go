package opa

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
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

func TestHandlerAllowsRequestAndSendsOPAInput(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodPost {
			t.Fatalf("method = %q, want POST", got)
		}
		if got := r.URL.Path; got != "/v1/data/http/authz" {
			t.Fatalf("path = %q, want /v1/data/http/authz", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode OPA body: %v", err)
		}
		input := body["input"].(map[string]any)
		if got := input["type"]; got != "http" {
			t.Fatalf("input.type = %v, want http", got)
		}
		request := input["request"].(map[string]any)
		if got := request["method"]; got != http.MethodGet {
			t.Fatalf("request.method = %v, want GET", got)
		}
		if got := request["path"]; got != "/get" {
			t.Fatalf("request.path = %v, want /get", got)
		}
		if got := request["host"]; got != "example.com" {
			t.Fatalf("request.host = %v, want example.com", got)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:   opa.URL,
		Policy: "http/authz",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsWithOPAStatusReasonAndHeaders(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(
			[]byte(
				`{"result":{"allow":false,"status_code":401,"reason":"no token","headers":{"WWW-Authenticate":"Bearer"}}}`,
			),
		)
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:   opa.URL,
		Policy: "authz",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if got := res.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "no token" {
		t.Fatalf("body = %q, want no token", got)
	}
}

func TestHandlerCopiesAllowedHeadersToUpstreamAndClearsAbsentOnes(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":{"allow":true,"headers":{"X-User":"alice"}}}`))
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:                opa.URL,
		Policy:              "authz",
		SendHeadersUpstream: []string{"X-User", "X-Role"},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("X-Role", "client-role")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-User"); got != "alice" {
			t.Fatalf("X-User = %q, want alice", got)
		}
		if got := r.Header.Get("X-Role"); got != "" {
			t.Fatalf("X-Role = %q, want cleared", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerReturns503ForInvalidDecision(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:   opa.URL,
		Policy: "authz",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func performRequest(p *Plugin, upstream func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?x=1", nil)
	req.Header.Set("X-Test", "yes")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(upstream)).ServeHTTP(rr, req)
	return rr
}
