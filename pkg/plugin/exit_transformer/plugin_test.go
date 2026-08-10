package exit_transformer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestPostInitRejectsNonFunctionLua(t *testing.T) {
	p := &Plugin{config: Config{Functions: []string{`return 42`}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "only accept Lua function") {
		t.Fatalf("PostInit() error = %v", err)
	}
}

func TestHandlerExecutesGeneralLuaControlFlow(t *testing.T) {
	p := newTestPlugin(t, Config{Functions: []string{`
		return function(code, body, header)
			for i = 1, 2 do code = code + 1 end
			header["X-Transformed"] = tostring(code)
			return code, body, header
		end
	`}})
	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	if res.Code != http.StatusPaymentRequired || res.Header().Get("X-Transformed") != "402" {
		t.Fatalf("response = %d/%q", res.Code, res.Header().Get("X-Transformed"))
	}
}

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

func TestHandlerNilBodyPreservesRepresentationHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{Functions: []string{
		`return function(code, body, header) return code, nil, header end`,
	}})

	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		setExitRepresentationHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	if got := res.Body.String(); got != "upstream" {
		t.Fatalf("body = %q, want upstream", got)
	}
	for _, field := range exitRepresentationHeaders() {
		if got := res.Header().Get(field); got != "stale" {
			t.Errorf("%s = %q, want stale header preserved", field, got)
		}
	}
}

func TestHandlerBodyReplacementInvalidatesRepresentationHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{Functions: []string{
		`return function(code, body, header) return code, "replacement", header end`,
	}})

	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		setExitRepresentationHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	if got := res.Body.String(); got != "replacement" {
		t.Fatalf("body = %q, want replacement", got)
	}
	for _, field := range exitRepresentationHeaders() {
		if values := res.Header().Values(field); len(values) != 0 {
			t.Errorf("%s = %v, want removed after body replacement", field, values)
		}
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

func exitRepresentationHeaders() []string {
	return []string{
		"Content-Length", "Content-Encoding", "Content-Range", "Content-MD5",
		"Digest", "Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
	}
}

func setExitRepresentationHeaders(header http.Header) {
	for _, field := range exitRepresentationHeaders() {
		header.Set(field, "stale")
	}
}

func TestExitTransformerCompilesFunctionsOnceOutsideRequestPath(t *testing.T) {
	before := compileFunctionCount.Load()
	p := newTestPlugin(t, Config{Functions: []string{
		`return function(code, body, header) return code, body, header end`,
		`return function(code, body, header) header["X-B"] = "b" return code, body, header end`,
	}})
	compiledDuringInit := compileFunctionCount.Load() - before
	if compiledDuringInit != 2 {
		t.Fatalf("compilations during PostInit = %d, want 2", compiledDuringInit)
	}

	for i := range 5 {
		res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		if res.Code != http.StatusOK {
			t.Fatalf("request %d code = %d, want 200", i+1, res.Code)
		}
	}
	if got := compileFunctionCount.Load() - before; got != 2 {
		t.Fatalf("compilations after 5 responses = %d, want the 2 static functions only", got)
	}
}

func TestExitTransformerConcurrentResponsesDoNotRecompile(t *testing.T) {
	before := compileFunctionCount.Load()
	p := newTestPlugin(t, Config{Functions: []string{
		`return function(code, body, header) return code, body, header end`,
	}})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			if res.Code != http.StatusOK {
				t.Errorf("concurrent response code = %d, want 200", res.Code)
			}
		})
	}
	wg.Wait()
	if got := compileFunctionCount.Load() - before; got != 1 {
		t.Fatalf("compilations under concurrency = %d, want 1", got)
	}
}
