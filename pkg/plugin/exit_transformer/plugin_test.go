package exit_transformer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestPostInitRejectsMalformedEquality(t *testing.T) {
	p := &Plugin{config: Config{Functions: []string{
		"return (function(code, body, header) if code == then return 405 end return code, body, header end)(...)",
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "unexpected symbol") {
		t.Fatalf("PostInit() error = %v, want unexpected symbol", err)
	}
}

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
		_, _ = w.Write([]byte(`{"message":"Missing API key in request"}`))
	})

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if got := res.Body.String(); got != `{"message":"Missing API key in request"}` {
		t.Fatalf("body = %q, want original body", got)
	}
}

func TestHandlerRemapsStatusAndBodyWithDocumentedLuaPattern(t *testing.T) {
	p := newTestPlugin(t, Config{Functions: []string{
		`return (function(code, body, header) if code == 503 then return 502, "Modified 503 to 502", header end return code, body, header end)(...)`,
	}})

	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.Code)
	}
	if got := res.Body.String(); got != "Modified 503 to 502" {
		t.Fatalf("body = %q, want transformed body", got)
	}
}

func TestHandlerRemapsStatusForDocumentedRequestContentTypeCondition(t *testing.T) {
	p := newTestPlugin(t, Config{Functions: []string{`
		return (function(code, body, header)
			local core = require("apisix.core")
			local ct = core.request.headers()["Content-Type"]
			if ct == "application/json" and code == 404 then return 405 end
			return code, body, header
		end)(...)
	`}})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/missing", nil)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(res, req)

	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.Code)
	}
}

func TestHandlerTransformsDocumentedErrorTable(t *testing.T) {
	p := newTestPlugin(t, Config{Functions: []string{`
		return (function(code, body, header)
			if code == 401 and body.message == "Missing API key in request" then
				return 400, {message = "authentication Failed"}, {["content-type"] = "application/json"}
			end
			return code, body, header
		end)(...)
	`}})

	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Missing API key in request"}`))
	})

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := res.Body.String(); got != `{"message":"authentication Failed"}` {
		t.Fatalf("body = %q, want transformed JSON body", got)
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
		_, _ = w.Write([]byte(`{"message":"Missing API key in request"}`))
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
		_, _ = w.Write([]byte(`{"message":"Missing API key in request"}`))
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
		_, _ = w.Write([]byte("ok"))
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
		_, _ = w.Write([]byte(`{"message":"upstream failure"}`))
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
