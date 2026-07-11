package ai_rate_limiting

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/ai_proxy_multi"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
)

func newTestPlugin(t *testing.T, cfg Config, now func() time.Time) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg, now: now}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerChargesTotalTokensAndRejectsNextRequest(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		Limit:         10,
		TimeWindow:    60,
		RejectedCode:  http.StatusTooManyRequests,
		RejectedMsg:   "token quota exceeded",
		LimitStrategy: "total_tokens",
	}, func() time.Time { return now })

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":4,"completion_tokens":8,"total_tokens":12}}`))
	})

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-RateLimit-Limit-global"); got != "10" {
		t.Fatalf("limit header = %q, want 10", got)
	}
	if got := first.Header().Get("X-AI-RateLimit-Remaining-global"); got != "0" {
		t.Fatalf("remaining header = %q, want 0 after charging response tokens", got)
	}

	second := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second response code = %d, want 429", second.Code)
	}
	if !strings.Contains(second.Body.String(), "token quota exceeded") {
		t.Fatalf("second response body = %q, want custom rejection message", second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want only first request to pass", calls)
	}
}

func TestHandlerUsesInstancePromptTokenQuota(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		LimitStrategy: "prompt_tokens",
		Instances: []InstanceLimit{
			{Name: "deepseek-main", Limit: 5, TimeWindow: 30},
		},
	}, func() time.Time { return now })

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":20,"total_tokens":23}}`))
	})

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		req := WithPickedAIInstanceName(
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			"deepseek-main",
		)
		p.Handler(upstream).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("response %d code = %d, want 200", i+1, rr.Code)
		}
	}

	rr := httptest.NewRecorder()
	req := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "deepseek-main")
	p.Handler(upstream).ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("third response code = %d, want default 503", rr.Code)
	}
	if got := rr.Header().Get("X-AI-RateLimit-Limit-deepseek-main"); got != "5" {
		t.Fatalf("instance limit header = %q, want 5", got)
	}
}

func TestHandlerSkipsUnconfiguredInstance(t *testing.T) {
	p := newTestPlugin(t, Config{
		LimitStrategy: "total_tokens",
		Instances: []InstanceLimit{
			{Name: "limited", Limit: 1, TimeWindow: 60},
		},
	}, time.Now)

	req := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "other")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":100}}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want pass-through for unconfigured instance", rr.Code)
	}
}

func TestHandlerResetsQuotaAfterWindow(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		Limit:      1,
		TimeWindow: 1,
	}, func() time.Time { return now })

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	p.Handler(upstream).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, httptest.NewRequest(http.MethodPost, "/", nil))
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}

	now = now.Add(2 * time.Second)
	allowed := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(allowed, httptest.NewRequest(http.MethodPost, "/", nil))
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed response code = %d, want 200 after reset", allowed.Code)
	}
}

func TestAIProxyAndRateLimiterExecuteInAPISIXPhaseOrder(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	proxy := &ai_proxy.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy.Config) = ai_proxy.Config{
		Provider: "openai-compatible",
		Auth:     ai_proxy.Auth{Header: map[string]string{"Authorization": "Bearer test"}},
		Override: ai_proxy.Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	rate := newTestPlugin(t, Config{Limit: 2, TimeWindow: 60}, time.Now)
	fallbackCalls := 0
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		fallbackCalls++
	})))))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		  "messages":[{"role":"user","content":"ping"}]
		}`))
		req = apisixctx.WithRequestVars(req)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-RateLimit-Remaining-ai-proxy-openai-compatible"); got != "0" {
		t.Fatalf("remaining header = %q, want 0", got)
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if upstreamCalls.Load() != 1 || fallbackCalls != 0 {
		t.Fatalf("upstream calls = %d, fallback calls = %d, want 1 and 0", upstreamCalls.Load(), fallbackCalls)
	}
}

func TestAIProxyStreamingResponseIsFlushedAndCharged(t *testing.T) {
	var upstreamCalls atomic.Int64
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"gpt-stream\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	proxy := &ai_proxy.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy.Config) = ai_proxy.Config{
		Provider: "openai-compatible",
		Override: ai_proxy.Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	rate := newTestPlugin(t, Config{Limit: 2, TimeWindow: 60}, time.Now)
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		t.Fatal("ordinary upstream called for streaming AI request")
	})))))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		  "messages":[{"role":"user","content":"ping"}],
		  "stream":true
		}`))
		req = apisixctx.WithRequestVars(req)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	if first.Code != http.StatusOK || first.Body.String() != streamBody {
		t.Fatalf("first response = (%d, %q), want exact stream", first.Code, first.Body.String())
	}
	if !first.Flushed {
		t.Fatal("streaming response was buffered by rate limiter")
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
}

func TestAIProxyMultiPublishesInstanceBeforeRateLimitPreflight(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	proxy := &ai_proxy_multi.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy_multi.Config) = ai_proxy_multi.Config{Instances: []ai_proxy_multi.Instance{{
		Name:     "model-a",
		Provider: "openai-compatible",
		Weight:   1,
		Auth:     ai_proxy_multi.Auth{Header: map[string]string{"Authorization": "Bearer test"}},
		Override: ai_proxy_multi.Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	}}}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	rate := newTestPlugin(t, Config{Instances: []InstanceLimit{{Name: "model-a", Limit: 2, TimeWindow: 60}}}, time.Now)
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		t.Fatal("ordinary upstream called for AI request")
	})))))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		  "messages":[{"role":"user","content":"ping"}]
		}`))
		req = apisixctx.WithRequestVars(req)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-RateLimit-Remaining-model-a"); got != "0" {
		t.Fatalf("remaining header = %q, want 0", got)
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
}

func TestAIProxyMultiSkipsRateLimitedInstance(t *testing.T) {
	var firstCalls atomic.Int64
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`))
	}))
	defer firstUpstream.Close()
	var secondCalls atomic.Int64
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`))
	}))
	defer secondUpstream.Close()

	proxy := &ai_proxy_multi.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy_multi.Config) = ai_proxy_multi.Config{
		Instances: []ai_proxy_multi.Instance{
			{
				Name: "model-a", Provider: "openai-compatible", Weight: 1,
				Auth:     ai_proxy_multi.Auth{Header: map[string]string{"Authorization": "Bearer test"}},
				Override: ai_proxy_multi.Override{Endpoint: firstUpstream.URL + "/v1/chat/completions"},
			},
			{
				Name: "model-b", Provider: "openai-compatible", Weight: 1,
				Auth:     ai_proxy_multi.Auth{Header: map[string]string{"Authorization": "Bearer test"}},
				Override: ai_proxy_multi.Override{Endpoint: secondUpstream.URL + "/v1/chat/completions"},
			},
		},
		FallbackStrategy: []string{"rate_limiting"},
	}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	rate := newTestPlugin(t, Config{Instances: []InstanceLimit{
		{Name: "model-a", Limit: 1, TimeWindow: 60},
		{Name: "model-b", Limit: 5, TimeWindow: 60},
	}}, time.Now)
	rate.charge(quota{key: "instance:model-a", headerName: "model-a", limit: 1, window: time.Minute}, 1)
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		t.Fatal("ordinary upstream called for AI request")
	})))))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		  "messages":[{"role":"user","content":"ping"}]
		}`))
		req = apisixctx.WithRequestVars(req)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, request())
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed response code = %d, want 200", allowed.Code)
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf("provider calls = (%d, %d), want (0, 1)", firstCalls.Load(), secondCalls.Load())
	}
	if got := allowed.Header().Get("X-AI-RateLimit-Remaining-model-b"); got != "4" {
		t.Fatalf("model-b remaining header = %q, want 4", got)
	}

	rate.charge(quota{key: "instance:model-b", headerName: "model-b", limit: 5, window: time.Minute}, 4)
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf("provider calls after rejection = (%d, %d), want (0, 1)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestPostInitAcceptsExpressionCostStrategy(t *testing.T) {
	p := &Plugin{config: Config{
		Limit:         10,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "prompt_tokens + completion_tokens",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
}

func TestHandlerResolvesGlobalQuotaFromRequestVariables(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"limit":"$http_x_limit","time_window":"${http_x_window}"}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	p := newTestPlugin(t, cfg, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Limit", "2")
	req.Header.Set("X-Window", "10")
	rr := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("X-AI-RateLimit-Limit-global"); got != "2" {
		t.Fatalf("limit header = %q, want 2", got)
	}
}

func TestHandlerResolvesInstanceQuotaFromRequestVariables(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{
		"instances":[{"name":"model-a","limit":"$http_x_limit","time_window":"$http_x_window"}]
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	p := newTestPlugin(t, cfg, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})

	req := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/", nil), "model-a")
	req.Header.Set("X-Limit", "3")
	req.Header.Set("X-Window", "10")
	rr := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("X-AI-RateLimit-Limit-model-a"); got != "3" {
		t.Fatalf("limit header = %q, want 3", got)
	}
}

func TestHandlerRejectsInvalidResolvedQuotaValues(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"limit":"$http_x_limit","time_window":"$http_x_window"}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	p := newTestPlugin(t, cfg, time.Now)

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Limit", "0")
	req.Header.Set("X-Window", "not-a-number")
	rr := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHandlerRejectsMalformedResolvedWindow(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:      "$http_x_limit",
		TimeWindow: "$http_x_window",
	}, time.Now)

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Limit", "1")
	req.Header.Set("X-Window", "not-a-number")
	rr := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHandlerAppliesSingleRule(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{
		{Count: 1, TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Tenant"},
	}}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Tenant", "team-a")
		return req
	}

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, request())
	if got := first.Header().Get("X-AI-Tenant-RateLimit-Limit"); got != "1" {
		t.Fatalf("tenant limit header = %q, want 1", got)
	}

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
}

func TestHandlerAppliesIndependentRulesWithRuleHeaders(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Count: 2, TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Tenant"},
			{Count: 5, TimeWindow: 60, Key: "$http_x_model"},
		},
	}, now: time.Now}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Tenant", "team-a")
		req.Header.Set("X-Model", "model-a")
		return req
	}

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		p.Handler(upstream).ServeHTTP(rr, request())
		if rr.Code != http.StatusOK {
			t.Fatalf("response %d code = %d, want 200", i+1, rr.Code)
		}
		if got := rr.Header().Get("X-AI-Tenant-RateLimit-Limit"); got != "2" {
			t.Fatalf("tenant limit header = %q, want 2", got)
		}
		if got := rr.Header().Get("X-AI-2-RateLimit-Limit"); got != "5" {
			t.Fatalf("default rule limit header = %q, want 5", got)
		}
	}

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if got := blocked.Header().Get("X-AI-2-RateLimit-Limit"); got != "" {
		t.Fatalf("later rule limit header = %q, want omitted after earlier rejection", got)
	}
}

func TestHandlerSkipsInvalidDynamicRuleAndAppliesValidRule(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{
		{Count: "$http_x_bad_count", TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Bad"},
		{Count: 1, TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Tenant"},
	}}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Bad-Count", "not-a-number")
		req.Header.Set("X-Tenant", "team-a")
		return req
	}

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, request())
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-Tenant-RateLimit-Limit"); got != "1" {
		t.Fatalf("valid rule limit header = %q, want 1", got)
	}
	if got := first.Header().Get("X-AI-Bad-RateLimit-Limit"); got != "" {
		t.Fatalf("invalid rule limit header = %q, want omitted", got)
	}

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
}

func TestHandlerReturnsInternalServerErrorWhenNoRuleResolves(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{
		{Count: "$http_x_bad_count", TimeWindow: 60, Key: "$http_x_tenant"},
	}}, time.Now)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Bad-Count", "not-a-number")
	req.Header.Set("X-Tenant", "team-a")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream called when no rule resolved")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
}

func TestResponseTokenCostEvaluatesExpressionAgainstRawUsage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:         10,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "math.max(prompt_tokens, math.abs(completion_tokens)) + missing_tokens + 0.6",
	}, time.Now)

	got := p.responseTokenCost([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":-4}}`))
	if got != 5 {
		t.Fatalf("expression cost = %d, want 5", got)
	}
}

func TestHandlerExpressionUsesRawUsageFromRequestContext(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:         20,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "input_tokens + output_tokens",
	}, time.Now)
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/", nil))
	apisixctx.RegisterRequestVar(req, "$llm_raw_usage", map[string]any{
		"input_tokens":  float64(6),
		"output_tokens": float64(4),
	})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("X-AI-RateLimit-Remaining-global"); got != "10" {
		t.Fatalf("remaining header = %q, want 10 from raw usage expression", got)
	}
}

func TestPostInitAcceptsAdditionalSafeMathFunctions(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:         20,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "math.sqrt(prompt_tokens) + math.pow(completion_tokens, 2)",
	}, time.Now)

	if got := p.responseTokenCost([]byte(`{"usage":{"prompt_tokens":9,"completion_tokens":2}}`)); got != 7 {
		t.Fatalf("expression cost = %d, want 7", got)
	}
}

func TestHandlerRejectsOverflowingDynamicWindow(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 1, TimeWindow: "$http_x_window"}, time.Now)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Window", "9223372036854775807")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream called for overflowing window")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
}

func TestResponseTokenCostClampsInvalidExpressionResults(t *testing.T) {
	for _, test := range []struct {
		name string
		expr string
		body string
	}{
		{name: "negative", expr: "-prompt_tokens", body: `{"usage":{"prompt_tokens":2}}`},
		{name: "non finite", expr: "1 / 0", body: `{"usage":{}}`},
		{name: "non numeric", expr: "prompt_tokens > 1", body: `{"usage":{"prompt_tokens":2}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Limit:         10,
				TimeWindow:    60,
				LimitStrategy: "expression",
				CostExpr:      test.expr,
			}, time.Now)

			if got := p.responseTokenCost([]byte(test.body)); got != 0 {
				t.Fatalf("expression cost = %d, want 0", got)
			}
		})
	}
}

func TestPostInitRejectsInvalidCostExpression(t *testing.T) {
	p := &Plugin{config: Config{
		Limit:         10,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "prompt_tokens +",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid cost expression error")
	}
}

func TestPostInitRequiresCostExpression(t *testing.T) {
	p := &Plugin{config: Config{
		Limit:         10,
		TimeWindow:    60,
		LimitStrategy: "expression",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want missing cost_expr error")
	}
}

func TestResponseTokenCostPreservesFixedStrategies(t *testing.T) {
	for _, test := range []struct {
		strategy string
		want     int64
	}{
		{strategy: "total_tokens", want: 9},
		{strategy: "prompt_tokens", want: 4},
		{strategy: "completion_tokens", want: 5},
	} {
		t.Run(test.strategy, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Limit:         10,
				TimeWindow:    60,
				LimitStrategy: test.strategy,
			}, time.Now)
			if got := p.responseTokenCost([]byte(`{"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9}}`)); got != test.want {
				t.Fatalf("responseTokenCost() = %d, want %d", got, test.want)
			}
		})
	}
}
