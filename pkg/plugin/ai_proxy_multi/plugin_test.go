package ai_proxy_multi

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/util"
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

func TestSchemaValidatesActiveHealthCheckFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"instances": []any{map[string]any{
			"name": "one", "provider": "openai-compatible", "weight": 1, "auth": map[string]any{},
			"checks": map[string]any{"active": map[string]any{
				"type": "http", "timeout": 0.5, "concurrency": 2, "http_path": "/health",
				"healthy": map[string]any{"successes": 2, "http_statuses": []any{200, 302}},
			}},
		}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("valid active health check rejected: %v", err)
	}
	config["instances"].([]any)[0].(map[string]any)["checks"].(map[string]any)["active"].(map[string]any)["type"] = "grpc"
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("unsupported active health check type was accepted")
	}
}

func TestHandlerRoundRobinBalancesAcrossInstances(t *testing.T) {
	var oneCalls atomic.Int64
	var twoCalls atomic.Int64

	one := newLLMServer(t, "one", "Bearer one-token", &oneCalls, http.StatusOK)
	defer one.Close()
	two := newLLMServer(t, "two", "Bearer two-token", &twoCalls, http.StatusOK)
	defer two.Close()

	p := newTestPlugin(t, Config{
		Balancer: Balancer{Algorithm: "roundrobin"},
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer one-token"}},
				Options:  map[string]any{"model": "gpt-4"},
				Override: Override{Endpoint: one.URL + "/v1/chat/completions"},
			},
			{
				Name:     "two",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer two-token"}},
				Options:  map[string]any{"model": "gpt-4o"},
				Override: Override{Endpoint: two.URL + "/v1/chat/completions"},
			},
		},
	})

	first := serveChat(t, p, "")
	second := serveChat(t, p, "")

	if oneCalls.Load() != 1 || twoCalls.Load() != 1 {
		t.Fatalf("upstream calls one=%d two=%d, want one call each", oneCalls.Load(), twoCalls.Load())
	}
	if first == second {
		t.Fatalf("round-robin responses = %q and %q, want different instances", first, second)
	}
}

func TestHandlerRetriesHTTP5xxFallback(t *testing.T) {
	var oneCalls atomic.Int64
	var twoCalls atomic.Int64

	one := newLLMServer(t, "one", "Bearer one-token", &oneCalls, http.StatusInternalServerError)
	defer one.Close()
	two := newLLMServer(t, "two", "Bearer two-token", &twoCalls, http.StatusOK)
	defer two.Close()

	p := newTestPlugin(t, Config{
		Balancer:         Balancer{Algorithm: "roundrobin"},
		FallbackStrategy: "http_5xx",
		MaxRetries:       intPtr(1),
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer one-token"}},
				Override: Override{Endpoint: one.URL + "/v1/chat/completions"},
			},
			{
				Name:     "two",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer two-token"}},
				Override: Override{Endpoint: two.URL + "/v1/chat/completions"},
			},
		},
	})

	body := serveChat(t, p, "")

	if oneCalls.Load() != 1 || twoCalls.Load() != 1 {
		t.Fatalf("upstream calls one=%d two=%d, want fallback to second instance", oneCalls.Load(), twoCalls.Load())
	}
	if !strings.Contains(body, `"instance":"two"`) {
		t.Fatalf("response body = %q, want second instance response", body)
	}
}

func TestHandlerStreamsFallbackResponseAndRegistersUsage(t *testing.T) {
	var failedCalls atomic.Int64
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failedCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failed.Close()

	var streamCalls atomic.Int64
	streamBody := "data: {\"id\":\"one\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"gpt-stream\",\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	streaming := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode streaming request: %v", err)
		}
		streamOptions, _ := body["stream_options"].(map[string]any)
		if streamOptions["include_usage"] != true {
			t.Fatalf("stream_options = %#v, want include_usage", body["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamBody))
	}))
	defer streaming.Close()

	p := newTestPlugin(t, Config{
		FallbackStrategy: "http_5xx",
		MaxRetries:       intPtr(1),
		Instances: []Instance{
			{
				Name: "failed", Provider: "openai-compatible", Priority: 10, Weight: 1,
				Override: Override{Endpoint: failed.URL + "/v1/chat/completions"},
			},
			{
				Name: "streaming", Provider: "openai-compatible", Priority: 0, Weight: 1,
				Override: Override{Endpoint: streaming.URL + "/v1/chat/completions"},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"gpt-request",
	  "messages":[{"role":"user","content":"hello"}],
	  "stream":true
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for multi proxy stream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || rr.Body.String() != streamBody {
		t.Fatalf("response = (%d, %q), want exact stream", rr.Code, rr.Body.String())
	}
	if failedCalls.Load() != 1 || streamCalls.Load() != 1 {
		t.Fatalf("provider calls = (%d, %d), want one each", failedCalls.Load(), streamCalls.Load())
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_stream")
	assertLLMRequestVar(t, req, "$request_llm_model", "gpt-request")
	assertLLMRequestVar(t, req, "$llm_model", "gpt-stream")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(4))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(2))
	assertUsageRequestVars(t, req, float64(4), int64(6))
}

func TestHandlerConvertsAnthropicRequestForSuccessfulFallbackInstance(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failed.Close()
	converted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode converted OpenAI request: %v", err)
		}
		if body["max_completion_tokens"] != float64(32) {
			t.Fatalf("converted request = %#v", body)
		}
		_, _ = w.Write([]byte(`{
		  "id":"chat-1","model":"provider-model",
		  "choices":[{"finish_reason":"stop","message":{"content":"hello"}}],
		  "usage":{"prompt_tokens":3,"completion_tokens":1}
		}`))
	}))
	defer converted.Close()

	p := newTestPlugin(t, Config{
		FallbackStrategy: "http_5xx",
		MaxRetries:       intPtr(1),
		Instances: []Instance{
			{
				Name: "failed", Provider: "openai-compatible", Priority: 10, Weight: 1,
				Override: Override{Endpoint: failed.URL + "/v1/chat/completions"},
			},
			{
				Name: "converted", Provider: "openai-compatible", Priority: 0, Weight: 1,
				Override: Override{Endpoint: converted.URL + "/v1/chat/completions"},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
	  "model":"client-model",
	  "max_tokens":32,
	  "messages":[{"role":"user","content":"hello"}]
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for converted Anthropic fallback")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, body = %q", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic response: %v", err)
	}
	content := response["content"].([]any)[0].(map[string]any)
	if response["type"] != "message" || response["model"] != "provider-model" || content["text"] != "hello" {
		t.Fatalf("converted Anthropic response = %#v", response)
	}
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(3))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(1))
}

func TestHandlerConvertsAnthropicStreamForFallbackInstance(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failed.Close()
	streaming := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"id\":\"chat-1\",\"model\":\"gpt-stream\",\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer streaming.Close()

	p := newTestPlugin(t, Config{
		FallbackStrategy: "http_5xx",
		MaxRetries:       intPtr(1),
		Instances: []Instance{
			{
				Name: "failed", Provider: "openai-compatible", Priority: 10, Weight: 1,
				Override: Override{Endpoint: failed.URL + "/v1/chat/completions"},
			},
			{
				Name: "streaming", Provider: "openai-compatible", Priority: 0, Weight: 1,
				Override: Override{Endpoint: streaming.URL + "/v1/chat/completions"},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
	  "model":"client-model","max_tokens":32,"stream":true,
	  "messages":[{"role":"user","content":"hello"}]
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for converted Anthropic fallback stream")
	})).ServeHTTP(rr, req)

	output := rr.Body.String()
	for _, expected := range []string{"event: message_start", `"type":"text_delta"`, "event: message_stop"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("converted stream missing %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, `"choices"`) || strings.Contains(output, "data: [DONE]") {
		t.Fatalf("OpenAI stream leaked through multi proxy:\n%s", output)
	}
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(2))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(1))
}

func TestHandlerEnforcesStreamDurationForSelectedInstance(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	flushInterval := 0
	p := newTestPlugin(t, Config{
		MaxStreamDurationMS:      25,
		StreamingFlushIntervalMS: &flushInterval,
		Instances: []Instance{{
			Name: "bounded", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "messages":[{"role":"user","content":"hello"}],"stream":true
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	started := time.Now()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for bounded multi stream")
	})).ServeHTTP(rr, req)

	if time.Since(started) > time.Second || !rr.Flushed || !strings.Contains(rr.Body.String(), "first") {
		t.Fatalf(
			"bounded stream = (duration %s, flushed %v, body %q)",
			time.Since(started),
			rr.Flushed,
			rr.Body.String(),
		)
	}
	if apisixctx.GetRequestVar(req, "$llm_time_to_first_token") == nil ||
		apisixctx.GetRequestVar(req, "$llm_request_done") != true {
		t.Fatalf("timing vars = %#v", apisixctx.GetRequestVars(req))
	}
}

func TestHandlerPublishesConfiguredLoggingForSelectedInstance(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "model":"selected-model","choices":[{"message":{"content":"selected answer"}}],
		  "usage":{"prompt_tokens":4,"completion_tokens":1}
		}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Logging: Logging{Summaries: true, Payloads: true},
		Instances: []Instance{{
			Name: "selected", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		}},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"question"}]}`),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for AI proxy multi")
	})).ServeHTTP(rr, req)

	summary := apisixctx.GetRequestVar(req, "$llm_summary").(map[string]any)
	if summary["model"] != "selected-model" || summary["prompt_tokens"] != int64(4) {
		t.Fatalf("$llm_summary = %#v", summary)
	}
	if responseLog := apisixctx.GetRequestVar(req, "$llm_response").(map[string]any); responseLog["content"] != "selected answer" {
		t.Fatalf("$llm_response = %#v", responseLog)
	}
}

func TestHandlerExhaustsHigherPriorityBeforeFallback(t *testing.T) {
	var highCalls atomic.Int64
	var lowCalls atomic.Int64
	high := newLLMServer(t, "high", "Bearer high", &highCalls, http.StatusInternalServerError)
	defer high.Close()
	low := newLLMServer(t, "low", "Bearer low", &lowCalls, http.StatusOK)
	defer low.Close()

	p := newTestPlugin(t, Config{
		FallbackStrategy: "http_5xx",
		MaxRetries:       intPtr(1),
		Instances: []Instance{
			{
				Name: "low", Provider: "openai-compatible", Priority: 0, Weight: 100,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer low"}},
				Override: Override{Endpoint: low.URL + "/v1/chat/completions"},
			},
			{
				Name: "high", Provider: "openai-compatible", Priority: 10, Weight: 1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer high"}},
				Override: Override{Endpoint: high.URL + "/v1/chat/completions"},
			},
		},
	})

	body := serveChat(t, p, "")

	if highCalls.Load() != 1 || lowCalls.Load() != 1 {
		t.Fatalf("provider calls = (%d, %d), want high then low once", highCalls.Load(), lowCalls.Load())
	}
	if !strings.Contains(body, `"instance":"low"`) {
		t.Fatalf("response body = %q, want low-priority fallback response", body)
	}
}

func TestHandlerKeepsLowerPriorityIdleWhileHigherPriorityIsHealthy(t *testing.T) {
	var highCalls atomic.Int64
	var lowCalls atomic.Int64
	high := newLLMServer(t, "high", "Bearer high", &highCalls, http.StatusOK)
	defer high.Close()
	low := newLLMServer(t, "low", "Bearer low", &lowCalls, http.StatusOK)
	defer low.Close()

	p := newTestPlugin(t, Config{Instances: []Instance{
		{
			Name: "low", Provider: "openai-compatible", Priority: 0, Weight: 100,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer low"}},
			Override: Override{Endpoint: low.URL + "/v1/chat/completions"},
		},
		{
			Name: "high", Provider: "openai-compatible", Priority: 10, Weight: 1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer high"}},
			Override: Override{Endpoint: high.URL + "/v1/chat/completions"},
		},
	}})

	for range 3 {
		serveChat(t, p, "")
	}
	if highCalls.Load() != 3 || lowCalls.Load() != 0 {
		t.Fatalf("provider calls = (%d, %d), want (3, 0)", highCalls.Load(), lowCalls.Load())
	}
}

func TestHandlerSkipsActivelyUnhealthyHigherPriorityInstance(t *testing.T) {
	var highProviderCalls atomic.Int64
	high := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			if r.Header.Get("Authorization") != "Bearer high" || r.Header.Get("X-Health") != "probe" ||
				r.URL.Query().Get("api-key") != "query-secret" {
				t.Fatalf("health request = (%#v, %q)", r.Header, r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		highProviderCalls.Add(1)
		_, _ = w.Write([]byte(`{"instance":"high"}`))
	}))
	defer high.Close()
	var lowProviderCalls atomic.Int64
	low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lowProviderCalls.Add(1)
		_, _ = w.Write([]byte(`{"instance":"low"}`))
	}))
	defer low.Close()

	p := newTestPlugin(t, Config{Instances: []Instance{
		{
			Name: "high", Provider: "openai-compatible", Priority: 10, Weight: 1,
			Auth: Auth{
				Header: map[string]string{"Authorization": "Bearer high"},
				Query:  map[string]string{"api-key": "query-secret"},
			},
			Override: Override{Endpoint: high.URL + "/v1/chat/completions"},
			Checks: &HealthChecks{Active: ActiveHealthCheck{
				HTTPPath: "/health", ReqHeaders: []string{"X-Health: probe"},
				Unhealthy: UnhealthyCheckPolicy{HTTPStatuses: []int{500}, HTTPFailures: 1},
			}},
		},
		{
			Name: "low", Provider: "openai-compatible", Priority: 0, Weight: 1,
			Override: Override{Endpoint: low.URL + "/v1/chat/completions"},
		},
	}})

	body := serveChat(t, p, "")
	if highProviderCalls.Load() != 0 || lowProviderCalls.Load() != 1 || !strings.Contains(body, `"instance":"low"`) {
		t.Fatalf(
			"provider calls = (%d, %d), body = %q",
			highProviderCalls.Load(),
			lowProviderCalls.Load(),
			body,
		)
	}
}

func TestHandlerUsesDefaultPriorityWhenAllHealthChecksFail(t *testing.T) {
	newServer := func(name string, calls *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			calls.Add(1)
			_, _ = w.Write([]byte(`{"instance":"` + name + `"}`))
		}))
	}
	var highCalls atomic.Int64
	var lowCalls atomic.Int64
	high := newServer("high", &highCalls)
	defer high.Close()
	low := newServer("low", &lowCalls)
	defer low.Close()
	checks := func() *HealthChecks {
		return &HealthChecks{Active: ActiveHealthCheck{
			HTTPPath: "/health", Unhealthy: UnhealthyCheckPolicy{HTTPStatuses: []int{500}, HTTPFailures: 1},
		}}
	}
	p := newTestPlugin(t, Config{Instances: []Instance{
		{
			Name: "high", Provider: "openai-compatible", Priority: 10, Weight: 1,
			Override: Override{Endpoint: high.URL + "/v1/chat/completions"}, Checks: checks(),
		},
		{
			Name: "low", Provider: "openai-compatible", Priority: 0, Weight: 1,
			Override: Override{Endpoint: low.URL + "/v1/chat/completions"}, Checks: checks(),
		},
	}})

	body := serveChat(t, p, "")
	if highCalls.Load() != 1 || lowCalls.Load() != 0 || !strings.Contains(body, `"instance":"high"`) {
		t.Fatalf("provider calls = (%d, %d), body = %q", highCalls.Load(), lowCalls.Load(), body)
	}
}

func TestHandlerChashUsesHeaderKey(t *testing.T) {
	var oneCalls atomic.Int64
	var twoCalls atomic.Int64

	one := newLLMServer(t, "one", "Bearer one-token", &oneCalls, http.StatusOK)
	defer one.Close()
	two := newLLMServer(t, "two", "Bearer two-token", &twoCalls, http.StatusOK)
	defer two.Close()

	p := newTestPlugin(t, Config{
		Balancer: Balancer{Algorithm: "chash", HashOn: "header", Key: "X-Tenant"},
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer one-token"}},
				Override: Override{Endpoint: one.URL + "/v1/chat/completions"},
			},
			{
				Name:     "two",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer two-token"}},
				Override: Override{Endpoint: two.URL + "/v1/chat/completions"},
			},
		},
	})

	for range 4 {
		serveChat(t, p, "tenant-a")
	}

	if oneCalls.Load() != 0 && twoCalls.Load() != 0 {
		t.Fatalf(
			"chash calls one=%d two=%d, want same header to choose one stable instance",
			oneCalls.Load(),
			twoCalls.Load(),
		)
	}
}

func TestHashKeySupportsConsumerAndVariableCombinations(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/models?version=v1", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	req.Header.Set("X-Tenant", "tenant-a")
	req = apisixctx.WithApisixVars(req, map[string]string{"$consumer_name": "alice"})
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$request_llm_model", "gpt-4")

	tests := []struct {
		hashOn string
		key    string
		want   string
	}{
		{hashOn: "consumer", want: "alice"},
		{hashOn: "vars", key: "uri", want: "/models"},
		{hashOn: "vars", key: "request_llm_model", want: "gpt-4"},
		{hashOn: "vars_combinations", key: "$consumer_name:$request_llm_model:$uri", want: "alice:gpt-4:/models"},
		{hashOn: "vars_combinations", key: "literal", want: "203.0.113.7"},
	}

	for _, test := range tests {
		p := &Plugin{config: Config{Balancer: Balancer{Algorithm: "chash", HashOn: test.hashOn, Key: test.key}}}
		if got := p.hashKey(req); got != test.want {
			t.Fatalf("hashKey(%s, %q) = %q, want %q", test.hashOn, test.key, got, test.want)
		}
	}
}

func TestHashKeyFallsBackToRemoteAddress(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:4321"
	p := &Plugin{config: Config{Balancer: Balancer{Algorithm: "chash", HashOn: "header", Key: "X-Missing"}}}

	if got := p.hashKey(req); got != "198.51.100.9" {
		t.Fatalf("hashKey() = %q, want remote address IP", got)
	}
}

func TestHandlerMergesRequestBodyOverrideWithoutForce(t *testing.T) {
	var upstreamBody map[string]any
	upstream := newBodyCaptureLLMServer(t, "Bearer token", &upstreamBody)
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
				Override: Override{
					Endpoint: upstream.URL + "/v1/chat/completions",
					RequestBody: map[string]any{
						"openai-chat": map[string]any{
							"temperature": float64(0),
							"stream":      false,
							"metadata": map[string]any{
								"client":  "override",
								"gateway": "apisix-go",
							},
						},
					},
				},
			},
		},
	})

	serveChatWithBody(t, p, `{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1,
	  "metadata": {"client": "caller"}
	}`)

	if got := upstreamBody["temperature"]; got != float64(1) {
		t.Fatalf("temperature = %v, want client value to win without force", got)
	}
	if got := upstreamBody["stream"]; got != false {
		t.Fatalf("stream = %v, want override to fill missing field", got)
	}
	metadata, ok := upstreamBody["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want object", upstreamBody["metadata"])
	}
	if got := metadata["client"]; got != "caller" {
		t.Fatalf("metadata.client = %v, want caller", got)
	}
	if got := metadata["gateway"]; got != "apisix-go" {
		t.Fatalf("metadata.gateway = %v, want apisix-go", got)
	}
}

func TestHandlerForceMergesRequestBodyOverride(t *testing.T) {
	var upstreamBody map[string]any
	upstream := newBodyCaptureLLMServer(t, "Bearer token", &upstreamBody)
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
				Override: Override{
					Endpoint:                 upstream.URL + "/v1/chat/completions",
					RequestBodyForceOverride: boolPtr(true),
					RequestBody: map[string]any{
						"openai-chat": map[string]any{
							"temperature": float64(0),
							"metadata": map[string]any{
								"client":  "override",
								"gateway": "apisix-go",
							},
						},
					},
				},
			},
		},
	})

	serveChatWithBody(t, p, `{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1,
	  "metadata": {"client": "caller"}
	}`)

	if got := upstreamBody["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want override value with force", got)
	}
	metadata, ok := upstreamBody["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want object", upstreamBody["metadata"])
	}
	if got := metadata["client"]; got != "override" {
		t.Fatalf("metadata.client = %v, want override", got)
	}
	if got := metadata["gateway"]; got != "apisix-go" {
		t.Fatalf("metadata.gateway = %v, want apisix-go", got)
	}
}

func TestHandlerOmitsModelForAzureOpenAI(t *testing.T) {
	var upstreamBody map[string]any
	upstream := newBodyCaptureLLMServer(t, "Bearer azure-token", &upstreamBody)
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "azure",
				Provider: "azure-openai",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer azure-token"}},
				Options: map[string]any{
					"model":       "gpt-4",
					"temperature": float64(0),
				},
				Override: Override{
					Endpoint: upstream.URL + "/openai/deployments/gpt-4/chat/completions?api-version=2024-02-15-preview",
				},
			},
		},
	})

	serveChatWithBody(t, p, `{
	  "model": "caller-model",
	  "messages": [{"role": "user", "content": "ping"}]
	}`)

	if _, ok := upstreamBody["model"]; ok {
		t.Fatalf("upstream body model = %v, want omitted for azure-openai", upstreamBody["model"])
	}
	if got := upstreamBody["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want configured option", got)
	}
}

func TestHandlerRegistersNonStreamingLLMRequestVars(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
		  "model": "gpt-4-0613",
		  "usage": {"prompt_tokens": 23, "completion_tokens": 8, "total_tokens": 31}
		}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
				Options:  map[string]any{"model": "gpt-4"},
				Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "messages": [{"role": "user", "content": "ping"}]
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy-multi")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_chat")
	assertLLMRequestVar(t, req, "$request_llm_model", "gpt-4")
	assertLLMRequestVar(t, req, "$llm_model", "gpt-4-0613")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(23))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(8))
	assertUsageRequestVars(t, req, float64(23), int64(31))
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestHandlerConvertsSelectedVertexEmbeddingsInstance(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode Vertex request: %v", err)
		}
		instances := body["instances"].([]any)
		if len(body) != 1 || instances[0].(map[string]any)["content"] != "hello" {
			t.Fatalf("Vertex request = %#v", body)
		}
		_, _ = w.Write([]byte(`{
		  "predictions":[{"embeddings":{"values":[0.1,0.2],"statistics":{"token_count":3}}}]
		}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name:     "vertex-embeddings",
		Provider: "vertex-ai",
		Weight:   1,
		Options:  map[string]any{"model": "text-embedding-005"},
		Override: Override{Endpoint: upstream.URL + "/predict"},
	}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{
	  "input":"hello"
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for Vertex embeddings")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, body = %q", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode OpenAI embeddings response: %v", err)
	}
	if response["object"] != "list" || response["model"] != "text-embedding-005" {
		t.Fatalf("OpenAI embeddings response = %#v", response)
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_embeddings")
	assertLLMRequestVar(t, req, "$request_llm_model", "text-embedding-005")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(3))
}

func TestHandlerBuildsAndSignsBedrockConverseInstance(t *testing.T) {
	var upstreamBody map[string]any
	var authorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/model/claude/converse" {
			t.Fatalf("upstream path = %q", got)
		}
		authorization = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		_, _ = w.Write([]byte(`{"usage":{"inputTokens":2,"outputTokens":1,"totalTokens":3}}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name:         "bedrock-a",
		Provider:     "bedrock",
		ProviderConf: map[string]any{"region": "us-east-1"},
		Weight:       1,
		Auth: Auth{AWS: &ai_auth.AWSConfig{
			AccessKeyID: "key", SecretAccessKey: "secret", SessionToken: "session",
		}},
		Options:  map[string]any{"model": "claude"},
		Override: Override{Endpoint: upstream.URL, LLMOptions: LLMOptions{MaxTokens: 64}},
	}}})
	p.now = func() time.Time { return time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC) }
	req := httptest.NewRequest(http.MethodPost, "/model/claude/converse", strings.NewReader(`{
	  "model":"caller-model",
	  "messages":[{"role":"user","content":[{"text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for Bedrock multi proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=key/") {
		t.Fatalf("response code = %d, authorization = %q", rr.Code, authorization)
	}
	if _, ok := upstreamBody["model"]; ok {
		t.Fatalf("upstream model = %#v, want omitted", upstreamBody["model"])
	}
	if got := upstreamBody["inferenceConfig"].(map[string]any)["maxTokens"]; got != float64(64) {
		t.Fatalf("inferenceConfig.maxTokens = %#v, want 64", got)
	}
}

func TestHandlerForwardsSelectedBedrockEventStreamInstance(t *testing.T) {
	metadata := testAWSEventStreamFrame(map[string]string{
		":message-type": "event", ":event-type": "metadata",
	}, `{"usage":{"inputTokens":3,"outputTokens":1,"totalTokens":4}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/claude/converse-stream" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(metadata)
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name:         "bedrock-stream",
		Provider:     "bedrock",
		ProviderConf: map[string]any{"region": "us-east-1"},
		Weight:       1,
		Auth: Auth{AWS: &ai_auth.AWSConfig{
			AccessKeyID: "key", SecretAccessKey: "secret",
		}},
		Options:  map[string]any{"model": "claude"},
		Override: Override{Endpoint: upstream.URL},
	}}})
	req := httptest.NewRequest(http.MethodPost, "/model/claude/converse", strings.NewReader(`{
	  "messages":[{"role":"user","content":[{"text":"hello"}]}],
	  "stream":true
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for multi Bedrock stream")
	})).ServeHTTP(rr, req)

	if string(rr.Body.Bytes()) != string(metadata) || !rr.Flushed {
		t.Fatal("multi Bedrock EventStream was not preserved and flushed")
	}
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(3))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(1))
}

func TestHandlerAppliesGCPAccessTokenForSelectedInstance(t *testing.T) {
	var authorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name:     "vertex-a",
		Provider: "vertex-ai",
		Weight:   1,
		Auth:     Auth{GCP: &ai_auth.GCPConfig{ServiceAccountJSON: "test"}},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	}}})
	p.gcpTokens = fakeGCPTokenApplier{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for Vertex multi proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || authorization != "Bearer gcp-token" {
		t.Fatalf("response code = %d, authorization = %q", rr.Code, authorization)
	}
}

type fakeGCPTokenApplier struct{}

func (fakeGCPTokenApplier) Apply(
	_ context.Context,
	_ *http.Client,
	req *http.Request,
	_ ai_auth.GCPConfig,
) error {
	req.Header.Set("Authorization", "Bearer gcp-token")
	return nil
}

func TestHandlerRejectsOversizedBodyBeforeProxy(t *testing.T) {
	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
				Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
			},
		},
		MaxReqBodySize: 4,
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"messages":[]}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for oversized request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("response code = %d, want 413", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "request body exceeds max_req_body_size") {
		t.Fatalf("response body = %q, want size message", rr.Body.String())
	}
}

func TestPostInitRejectsOpenAICompatibleWithoutEndpoint(t *testing.T) {
	p := &Plugin{config: Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
			},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "override.endpoint is required") {
		t.Fatalf("PostInit() error = %v, want override endpoint error", err)
	}
}

func newLLMServer(t *testing.T, instance string, wantAuth string, calls *atomic.Int64, status int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Fatalf("%s upstream method = %s, want POST", instance, r.Method)
		}
		if got := r.URL.Path; got != "/v1/chat/completions" {
			t.Fatalf("%s upstream path = %s, want /v1/chat/completions", instance, got)
		}
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Fatalf("%s Authorization header = %q, want %q", instance, got, wantAuth)
		}

		var upstreamBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("%s decode upstream body: %v", instance, err)
		}
		if upstreamBody["messages"] == nil {
			t.Fatalf("%s upstream body missing messages: %#v", instance, upstreamBody)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"instance":"` + instance + `"}`))
	}))
}

func newBodyCaptureLLMServer(t *testing.T, wantAuth string, body *map[string]any) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Fatalf("Authorization header = %q, want %q", got, wantAuth)
		}
		if err := json.NewDecoder(r.Body).Decode(body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

func serveChat(t *testing.T, p *Plugin, tenant string) string {
	t.Helper()

	return serveChatWithBody(t, p, `{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1
	}`, tenant)
}

func serveChatWithBody(t *testing.T, p *Plugin, body string, tenant ...string) string {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1
	}`))
	if body != "" {
		req = httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	if len(tenant) > 0 && tenant[0] != "" {
		req.Header.Set("X-Tenant", tenant[0])
	}
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy-multi")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200, body %q", rr.Code, rr.Body.String())
	}

	return strings.TrimSpace(rr.Body.String())
}

func intPtr(v int) *int {
	return &v
}

func boolPtr(v bool) *bool {
	return &v
}

func testAWSEventStreamFrame(headers map[string]string, payload string) []byte {
	headerBytes := make([]byte, 0)
	for name, value := range headers {
		headerBytes = append(headerBytes, byte(len(name)))
		headerBytes = append(headerBytes, name...)
		headerBytes = append(headerBytes, 7)
		length := make([]byte, 2)
		binary.BigEndian.PutUint16(length, uint16(len(value)))
		headerBytes = append(headerBytes, length...)
		headerBytes = append(headerBytes, value...)
	}
	totalLength := 16 + len(headerBytes) + len(payload)
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headerBytes)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headerBytes...)
	frame = append(frame, payload...)
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(frame))
	return append(frame, crc...)
}

func assertLLMRequestVar(t *testing.T, req *http.Request, key string, want any) {
	t.Helper()

	if got := apisixctx.GetRequestVar(req, key); got != want {
		t.Fatalf("%s = %#v, want %#v", key, got, want)
	}
}

func assertUsageRequestVars(t *testing.T, req *http.Request, wantRawPrompt float64, wantNormalizedTotal int64) {
	t.Helper()
	raw, ok := apisixctx.GetRequestVar(req, "$llm_raw_usage").(map[string]any)
	if !ok || raw["prompt_tokens"] != wantRawPrompt {
		t.Fatalf("$llm_raw_usage = %#v, want prompt_tokens %v", raw, wantRawPrompt)
	}
	normalized, ok := apisixctx.GetRequestVar(req, "$ai_token_usage").(map[string]any)
	if !ok || normalized["total_tokens"] != wantNormalizedTotal {
		t.Fatalf("$ai_token_usage = %#v, want total_tokens %d", normalized, wantNormalizedTotal)
	}
}
