package serverless

import (
	"sync"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func newTestPlugin(t *testing.T, p *Plugin, cfg Config) *Plugin {
	t.Helper()

	p.config = cfg
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPreFunctionReturnsCodeAndBodyWithoutUpstream(t *testing.T) {
	p := newTestPlugin(t, NewPreFunction(), Config{
		Functions: []string{
			`return function(conf, ctx) return 418, "teapot" end`,
		},
	})

	upstreamCalled := false
	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	})

	if upstreamCalled {
		t.Fatal("upstream was called after serverless function returned a response")
	}
	if res.Code != http.StatusTeapot {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusTeapot)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "teapot" {
		t.Fatalf("body = %q, want teapot", got)
	}
}

func TestPreFunctionCanSetRequestHeaderAndContinue(t *testing.T) {
	p := newTestPlugin(t, NewPreFunction(), Config{
		Phase: "rewrite",
		Functions: []string{
			`return function(conf, ctx) ngx.req.set_header("X-Serverless-Path", ctx.curr_req_matched._path) end`,
		},
	})

	var gotHeader string
	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Serverless-Path")
		w.WriteHeader(http.StatusNoContent)
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
	if gotHeader != "/anything" {
		t.Fatalf("X-Serverless-Path = %q, want /anything", gotHeader)
	}
}

func TestPreFunctionPersistsExternalUserOnRequestContext(t *testing.T) {
	p := newTestPlugin(t, NewPreFunction(), Config{
		Functions: []string{
			`return function(conf, ctx) ctx.external_user = {team = {"cloud", "infra"}} end`,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/anything", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalUser, ok := apisixctx.GetApisixVar(r, "$external_user").(map[string]any)
		if !ok {
			t.Fatalf("$external_user = %#v, want object", apisixctx.GetApisixVar(r, "$external_user"))
		}
		team, ok := externalUser["team"].([]any)
		if !ok || len(team) != 2 || team[0] != "cloud" || team[1] != "infra" {
			t.Fatalf("$external_user.team = %#v, want [cloud infra]", externalUser["team"])
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestPostFunctionCanRewriteBodyFilterJSONBody(t *testing.T) {
	p := newTestPlugin(t, NewPostFunction(), Config{
		Phase: "body_filter",
		Functions: []string{
			`return function(conf, ctx)
				local cjson = require("cjson")
				local core = require("apisix.core")
				local body = core.response.hold_body_chunk(ctx)
				if not body then
					return
				end
				body = cjson.decode(body)
				body.origin = nil
				body = cjson.encode(body)
				ngx.arg[1] = body
			end`,
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "29")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"origin":"127.0.0.1","ok":true}`))
	})

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusOK, res.Body.String())
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after body rewrite", got)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if _, ok := body["origin"]; ok {
		t.Fatalf("body = %#v, want origin removed", body)
	}
	if body["ok"] != true {
		t.Fatalf("body = %#v, want ok=true preserved", body)
	}
}

func TestPostFunctionCanOverrideCapturedResponseStatus(t *testing.T) {
	p := newTestPlugin(t, NewPostFunction(), Config{
		Phase: "body_filter",
		Functions: []string{
			`return function(conf, ctx) ngx.status = 418 end`,
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream body"))
	})

	if res.Code != http.StatusTeapot {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusTeapot)
	}
}

func TestPostInitRejectsLuaChunkThatDoesNotReturnFunction(t *testing.T) {
	p := NewPreFunction()
	p.config = Config{
		Functions: []string{`local count = 1`},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := p.PostInit()
	if err == nil {
		t.Fatal("PostInit() error = nil, want non-function Lua chunk rejected")
	}
	if !strings.Contains(err.Error(), "only accept Lua function") {
		t.Fatalf("PostInit() error = %v, want only accept Lua function", err)
	}
}

func performRequest(p *Plugin, upstream func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/anything?name=apisix", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(upstream)).ServeHTTP(rr, req)
	return rr
}

func TestServerlessCompilesFunctionsOnceOutsideRequestPath(t *testing.T) {
	before := compileFunctionCount.Load()
	p := newTestPlugin(t, NewPreFunction(), Config{
		Phase: "access",
		Functions: []string{
			`return function() ngx.say("first") end`,
			`return function() ngx.say("second") end`,
		},
	})
	compiledDuringInit := compileFunctionCount.Load() - before
	if compiledDuringInit != 2 {
		t.Fatalf("compilations during PostInit = %d, want 2", compiledDuringInit)
	}

	for i := 0; i < 5; i++ {
		res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		if res.Code != http.StatusOK {
			t.Fatalf("request %d code = %d, want 200; body=%s", i+1, res.Code, res.Body.String())
		}
	}
	if got := compileFunctionCount.Load() - before; got != 2 {
		t.Fatalf("compilations after 5 requests = %d, want the 2 static functions only", got)
	}
}

func TestServerlessConcurrentRequestsDoNotRecompile(t *testing.T) {
	before := compileFunctionCount.Load()
	p := newTestPlugin(t, NewPreFunction(), Config{
		Phase:    "access",
		Functions: []string{`return function() ngx.say("ok") end`},
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			if res.Code != http.StatusOK {
				t.Errorf("concurrent request code = %d, want 200", res.Code)
			}
		}()
	}
	wg.Wait()
	if got := compileFunctionCount.Load() - before; got != 1 {
		t.Fatalf("compilations under concurrency = %d, want 1", got)
	}
}
