package exit_transformer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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

func TestHandlerRemapsStatusWithDocumentedLuaPattern(t *testing.T) {
	p := newTestPlugin(t, Config{
		Functions: []string{
			"return (function(code, body, header) if code == 401 then return 403, body, header end return code, body, header end)(...)",
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Missing API key in request"}`))
	})

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if got := res.Body.String(); got != `{"message":"Missing API key in request"}` {
		t.Fatalf("body = %q, want original body", got)
	}
}

func TestHandlerNormalizesErrorBodyAndHeaderWithDocumentedLuaPattern(t *testing.T) {
	p := newTestPlugin(t, Config{
		Functions: []string{
			`return (function(code, body, header) if code and code >= 400 then header = header or {} header["X-Error-Code"] = tostring(code) body = {error = true, status = code, message = (type(body) == "table" and body.message) or "request failed"} end return code, body, header end)(...)`,
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Missing API key in request"}`))
	})

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
	if got := res.Header().Get("X-Error-Code"); got != "401" {
		t.Fatalf("X-Error-Code = %q, want 401", got)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != true || body["status"] != float64(401) || body["message"] != "Missing API key in request" {
		t.Fatalf("body = %#v, want normalized error payload", body)
	}
}

func TestHandlerChainsTransformers(t *testing.T) {
	p := newTestPlugin(t, Config{
		Functions: []string{
			"return (function(code, body, header) if code == 401 then return 403, body, header end return code, body, header end)(...)",
			`return (function(code, body, header) if code and code >= 400 then header = header or {} header["X-Error-Code"] = tostring(code) body = {error = true, status = code, message = (type(body) == "table" and body.message) or "request failed"} end return code, body, header end)(...)`,
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Missing API key in request"}`))
	})

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if got := res.Header().Get("X-Error-Code"); got != "403" {
		t.Fatalf("X-Error-Code = %q, want 403", got)
	}
}

func TestHandlerKeepsSuccessfulResponse(t *testing.T) {
	p := newTestPlugin(t, Config{
		Functions: []string{
			`return (function(code, body, header) if code and code >= 400 then header = header or {} header["X-Error-Code"] = tostring(code) body = {error = true, status = code, message = "request failed"} end return code, body, header end)(...)`,
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Body.String(); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
}

func TestHandlerDoesNotTransformKnownUpstreamResponse(t *testing.T) {
	p := newTestPlugin(t, Config{
		Functions: []string{
			`return (function(code, body, header) if code and code >= 400 then header = header or {} header["X-Error-Code"] = tostring(code) body = {error = true, status = code, message = "request failed"} end return code, body, header end)(...)`,
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/anything", nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$response_source", "upstream")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"message":"upstream failure"}`))
	})).ServeHTTP(rr, req)

	if got := rr.Body.String(); got != `{"message":"upstream failure"}` {
		t.Fatalf("body = %q, want upstream body unchanged", got)
	}
	if got := rr.Header().Get("X-Error-Code"); got != "" {
		t.Fatalf("X-Error-Code = %q, want empty for upstream response", got)
	}
}

func performRequest(p *Plugin, handler func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/anything", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(handler)).ServeHTTP(rr, req)
	return rr
}
