package serverless

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
	lua "github.com/yuin/gopher-lua"
)

func TestSchemaMatchesAPISIX317ServerlessMatrix(t *testing.T) {
	p := NewPreFunction()
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name   string
		config map[string]any
		valid  bool
	}{
		{
			name:   "default access phase",
			config: map[string]any{"functions": []any{`return function() end`}},
			valid:  true,
		},
		{
			name: "explicit rewrite phase",
			config: map[string]any{
				"phase":     "rewrite",
				"functions": []any{`return function() end`},
			},
			valid: true,
		},
		{
			name: "invalid phase",
			config: map[string]any{
				"phase":     "abc",
				"functions": []any{`return function() end`},
			},
		},
		{name: "missing functions", config: map[string]any{}},
		{name: "empty functions", config: map[string]any{"functions": []any{}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := util.Validate(test.config, p.GetSchema())
			if test.valid && err != nil {
				t.Fatalf("valid APISIX 3.17 configuration rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid APISIX 3.17 configuration accepted")
			}
		})
	}
}

func TestPostInitInterruptsInfiniteTopLevelChunk(t *testing.T) {
	p := NewPreFunction()
	p.config = Config{Functions: []string{`while true do end`}}
	p.SetDependencies(base.Dependencies{Config: serverlessStaticConfig(20)})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.PostInit() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("PostInit() error = %v, want deadline interruption", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("PostInit() did not interrupt infinite top-level chunk")
	}
}

func TestRequestInterruptsInfiniteServerlessFunction(t *testing.T) {
	p := newTestPluginWithStaticConfig(t, NewPreFunction(), Config{
		Functions: []string{`return function() while true do end end`},
	}, serverlessStaticConfig(20))
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- performRequest(p, http.NotFound) }()
	select {
	case result := <-response:
		if result.Code != http.StatusInternalServerError ||
			!strings.Contains(result.Body.String(), "deadline exceeded") {
			t.Fatalf("response = %d %q, want deadline error", result.Code, result.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("request did not interrupt infinite serverless function")
	}
}

func TestRequestCancellationPreemptsServerlessHardDeadline(t *testing.T) {
	p := newTestPluginWithStaticConfig(t, NewPreFunction(), Config{
		Functions: []string{`return function() while true do end end`},
	}, serverlessStaticConfig(1000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "http://example.com/anything", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "context canceled") {
		t.Fatalf("response = %d %q, want caller cancellation", response.Code, response.Body.String())
	}
}

func TestServerlessDoesNotExposeProcessOrFilesystemLibraries(t *testing.T) {
	p := newTestPlugin(t, NewPreFunction(), Config{Functions: []string{
		`return function()
			if os ~= nil or io ~= nil or dofile ~= nil or loadfile ~= nil or package.loaders[2] ~= nil then
				error("unsafe library exposed")
			end
		end`,
	}})
	result := performRequest(p, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if result.Code != http.StatusNoContent {
		t.Fatalf("response = %d %q, want safe library set", result.Code, result.Body.String())
	}
}

func TestPostInitRejectsInvalidOperatorExecutionTimeout(t *testing.T) {
	for _, timeout := range []any{0, 10001, int64(math.MaxInt64)} {
		p := NewPreFunction()
		p.config = Config{Functions: []string{`return function() end`}}
		p.SetDependencies(base.Dependencies{Config: serverlessStaticConfig(timeout)})
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "execution_timeout_ms") {
			t.Fatalf("PostInit(timeout=%v) error = %v, want operator timeout rejection", timeout, err)
		}
	}
}

func serverlessStaticConfig(timeoutMS any) *config.EffectiveConfig {
	return &config.EffectiveConfig{Config: config.Config{PluginAttr: map[string]map[string]any{
		preFunctionName: {"execution_timeout_ms": timeoutMS},
	}}}
}

func TestServerlessDescriptorSelectsOneConfiguredStageOrPhase(t *testing.T) {
	tests := []struct {
		phase      string
		wantStage  string
		wantHeader bool
		wantBody   bool
	}{
		{phase: "", wantStage: "access"},
		{phase: "access", wantStage: "access"},
		{phase: "rewrite", wantStage: "rewrite"},
		{phase: "before_proxy", wantStage: "before_proxy"},
		{phase: "header_filter", wantStage: "none", wantHeader: true},
		{phase: "body_filter", wantStage: "none", wantBody: true},
		{phase: "log", wantStage: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			plugin := newTestPlugin(t, NewPreFunction(), Config{
				Phase:     tt.phase,
				Functions: []string{`return function() end`},
			})
			descriptor, err := plugin.Config().(base.BindingPhaseDescriber).DescribeBindingPhases()
			if err != nil {
				t.Fatalf("DescribeBindingPhases() error = %v", err)
			}
			if descriptor.RequestStage != tt.wantStage || descriptor.Header != tt.wantHeader ||
				descriptor.BufferedBody != tt.wantBody {
				t.Fatalf(
					"descriptor = %+v, want stage=%q header=%t body=%t",
					descriptor,
					tt.wantStage,
					tt.wantHeader,
					tt.wantBody,
				)
			}
		})
	}
}

func TestServerlessLogPhaseRunsDetachedAndRejectsResponseMutation(t *testing.T) {
	plugin := newTestPlugin(t, NewPreFunction(), Config{
		Phase: "log",
		Functions: []string{`return function(conf, ctx)
			ngx.req.set_header("X-Detached", "yes")
		end`},
	})
	snapshot := base.BuildLogSnapshotFromOwnedInputs(
		httptest.NewRequest(http.MethodGet, "http://example.com/log", nil),
		base.ResponseCaptureSnapshot{Header: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("body")},
		nil,
		false,
		apisixctx.ResponseOutcome{Status: http.StatusOK},
		apisixctx.ResponseSourceUpstream,
		time.Time{}, time.Time{},
	)
	if err := plugin.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}

	mutating := newTestPlugin(t, NewPostFunction(), Config{
		Phase:     "log",
		Functions: []string{`return function() ngx.status = 418 end`},
	})
	if err := mutating.RunLogPhase(snapshot); err == nil || !strings.Contains(err.Error(), "response mutation") {
		t.Fatalf("RunLogPhase() error = %v, want bounded response mutation error", err)
	}
}

func TestServerlessLogPhaseReadsDetachedResponseSnapshot(t *testing.T) {
	plugin := newTestPlugin(t, NewPreFunction(), Config{
		Phase: "log",
		Functions: []string{`return function()
			if ngx.status ~= 418 or ngx.header["X-Final"] ~= "yes" or ngx.arg[1] ~= "final-body" then
				error("detached response snapshot missing")
			end
		end`},
	})
	snapshot := base.BuildLogSnapshotFromOwnedInputs(
		httptest.NewRequest(http.MethodGet, "http://example.com/log", nil),
		base.ResponseCaptureSnapshot{Header: http.Header{"X-Final": {"yes"}}, Body: []byte("final-body")},
		nil,
		false,
		apisixctx.ResponseOutcome{Status: http.StatusTeapot},
		apisixctx.ResponseSourceUpstream,
		time.Time{}, time.Time{},
	)
	if err := plugin.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
}

func TestServerlessLogPhasePolicyPreservesBodiesThroughDetachedClone(t *testing.T) {
	plugin := newTestPlugin(t, NewPreFunction(), Config{
		Phase:     "log",
		Functions: []string{`return function() end`},
	})
	policy := plugin.LogCapturePolicy()
	if policy.RequestBodyBytes != base.MAX_REQ_BODY || policy.ResponseBodyBytes != base.MAX_RESP_BODY {
		t.Fatalf("log capture policy = %+v, want bounded request/response limits", policy)
	}
	snapshot := base.BuildLogSnapshotFromOwnedInputs(
		httptest.NewRequest(http.MethodPost, "http://example.com/log", strings.NewReader("request-body")),
		base.ResponseCaptureSnapshot{Body: []byte("final-body")},
		[]byte("request-body"),
		false,
		apisixctx.ResponseOutcome{Status: http.StatusOK},
		apisixctx.ResponseSourceUpstream,
		time.Time{}, time.Time{},
	)
	cloned := base.CloneLogSnapshotForPolicy(snapshot, policy)
	if string(cloned.Request.Body) != "request-body" || string(cloned.Response.Body) != "final-body" {
		t.Fatalf("cloned bodies = %q/%q, want request-body/final-body", cloned.Request.Body, cloned.Response.Body)
	}
	if err := plugin.RunLogPhase(cloned); err != nil {
		t.Fatalf("RunLogPhase() with executor-style clone error = %v", err)
	}
}

func TestServerlessResponsePhaseCallbackMutatesOnlySelectedState(t *testing.T) {
	plugin := newTestPlugin(t, NewPostFunction(), Config{
		Phase: "body_filter",
		Functions: []string{`return function(conf, ctx)
			ngx.status = 418
			ngx.arg[1] = "body-filter"
		end`},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/anything", nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.SetRequestResponseSource(req, apisixctx.ResponseSourceAPISIX)
	state := base.ResponseState{
		Status: http.StatusOK,
		Header: http.Header{"Content-Length": {"8"}},
		Body:   []byte("upstream"),
	}
	if err := plugin.RunBufferedBodyFilter(req, &state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if state.Status != http.StatusTeapot || string(state.Body) != "body-filter" {
		t.Fatalf("state = %+v, want 418/body-filter", state)
	}
	if got := state.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want invalidated", got)
	}
}

func TestServerlessLuaStatusKeepsPreviousResponseStatusForInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "below minimum", value: "99"},
		{name: "informational", value: "100"},
		{name: "early informational", value: "103"},
		{name: "last informational", value: "199"},
		{name: "above maximum", value: "600"},
		{name: "far above maximum", value: "1000"},
		{name: "fractional", value: "418.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := newTestPlugin(t, NewPostFunction(), Config{
				Phase:     "body_filter",
				Functions: []string{"return function() ngx.status = " + tt.value + " end"},
			})
			req := httptest.NewRequest(http.MethodGet, "http://example.com/anything", nil)
			req = apisixctx.WithRequestVars(req)
			apisixctx.SetRequestResponseSource(req, apisixctx.ResponseSourceAPISIX)
			state := base.ResponseState{Status: http.StatusAccepted, Header: http.Header{}}

			if err := plugin.RunBufferedBodyFilter(req, &state); err != nil {
				t.Fatalf("RunBufferedBodyFilter() error = %v", err)
			}
			if state.Status != http.StatusAccepted {
				t.Fatalf("status = %d, want previous status %d", state.Status, http.StatusAccepted)
			}
		})
	}
}

func TestServerlessLuaStringStatusCanOverridePreviousResponseStatus(t *testing.T) {
	plugin := newTestPlugin(t, NewPostFunction(), Config{
		Phase:     "body_filter",
		Functions: []string{`return function() ngx.status = "418" end`},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/anything", nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.SetRequestResponseSource(req, apisixctx.ResponseSourceAPISIX)
	state := base.ResponseState{Status: http.StatusAccepted, Header: http.Header{}}

	if err := plugin.RunBufferedBodyFilter(req, &state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if state.Status != http.StatusTeapot {
		t.Fatalf("status = %d, want string status %d", state.Status, http.StatusTeapot)
	}
}

func TestServerlessLuaValueToStatusAcceptsOnlyHTTPIntegers(t *testing.T) {
	tests := []struct {
		name  string
		value lua.LValue
		valid bool
	}{
		{name: "below minimum", value: lua.LNumber(99)},
		{name: "above maximum", value: lua.LNumber(600)},
		{name: "far above maximum", value: lua.LNumber(1000)},
		{name: "nan", value: lua.LNumber(math.NaN())},
		{name: "fractional", value: lua.LNumber(418.5)},
		{name: "string below minimum", value: lua.LString("99")},
		{name: "string informational", value: lua.LString("100")},
		{name: "string early informational", value: lua.LString("103")},
		{name: "string last informational", value: lua.LString("199")},
		{name: "string above maximum", value: lua.LString("600")},
		{name: "string fractional", value: lua.LString("418.5")},
		{name: "invalid string", value: lua.LString("not-a-status")},
		{name: "string integer", value: lua.LString("418"), valid: true},
		{name: "minimum", value: lua.LNumber(200), valid: true},
		{name: "maximum", value: lua.LNumber(599), valid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, ok := luaValueToStatus(tt.value)
			if ok != tt.valid {
				t.Fatalf("luaValueToStatus() = %d, %t; valid = %t", status, ok, tt.valid)
			}
		})
	}
}

func newTestPlugin(t *testing.T, p *Plugin, cfg Config) *Plugin {
	return newTestPluginWithStaticConfig(t, p, cfg, nil)
}

func newTestPluginWithStaticConfig(
	t *testing.T,
	p *Plugin,
	cfg Config,
	effective *config.EffectiveConfig,
) *Plugin {
	t.Helper()

	p.config = cfg
	if effective != nil {
		p.SetDependencies(base.Dependencies{Config: effective})
	}
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

func TestAPISIX317PreFunctionNgxExitStopsWithStatus(t *testing.T) {
	p := newTestPlugin(t, NewPreFunction(), Config{
		Functions: []string{
			`return function() ngx.log(ngx.ERR, 'serverless pre function'); ngx.exit(201); end`,
		},
	})

	upstreamCalled := false
	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	})

	if upstreamCalled {
		t.Fatal("upstream was called after ngx.exit")
	}
	if res.Code != http.StatusCreated || res.Body.Len() != 0 {
		t.Fatalf("response = %d %q, want empty 201", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX 3.17 text/plain response", got)
	}
}

func TestAPISIX317FunctionsShareContextAndStopOnResponse(t *testing.T) {
	p := newTestPlugin(t, NewPreFunction(), Config{
		Functions: []string{
			`return function(conf, ctx) ctx.source_order = "first" end`,
			`return function(conf, ctx)
				if ctx.source_order ~= "first" then
					return 500, "missing shared context"
				end
				return 202, "second"
			end`,
			`return function() return 500, "unreachable" end`,
		},
	})

	upstreamCalled := false
	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	})

	if upstreamCalled {
		t.Fatal("upstream was called after the second serverless function returned a response")
	}
	if res.Code != http.StatusAccepted || res.Body.String() != "second" {
		t.Fatalf("response = %d %q, want 202 from the second function", res.Code, res.Body.String())
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

func TestAPISIX317RejectsInvalidLuaChunk(t *testing.T) {
	p := NewPreFunction()
	p.config = Config{Functions: []string{`a`}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "failed to loadstring") {
		t.Fatalf("PostInit() error = %v, want invalid Lua chunk rejection", err)
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

	for i := range 5 {
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
		Phase:     "access",
		Functions: []string{`return function() ngx.say("ok") end`},
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			if res.Code != http.StatusOK {
				t.Errorf("concurrent request code = %d, want 200", res.Code)
			}
		})
	}
	wg.Wait()
	if got := compileFunctionCount.Load() - before; got != 1 {
		t.Fatalf("compilations under concurrency = %d, want 1", got)
	}
}
