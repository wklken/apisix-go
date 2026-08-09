package ai_proxy_multi

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
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

func TestSchemaRejectsUnknownRequestBodyProtocolKey(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"instances": []any{map[string]any{
			"name": "one", "provider": "openai", "weight": 1,
			"auth": map[string]any{"header": map[string]any{"Authorization": "Bearer t"}},
			"override": map[string]any{
				"request_body": map[string]any{"not-a-protocol": map[string]any{"x": 1}},
			},
		}},
	}

	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("unknown request_body protocol key was accepted")
	}
}

func TestHandlerPreservesPassthroughRouting(t *testing.T) {
	type capturedRequest struct {
		method string
		uri    string
	}
	captured := make(chan capturedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{method: r.Method, uri: r.URL.RequestURI()}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{{
			Name:     "openai",
			Provider: "openai-compatible",
			Weight:   1,
			Auth: Auth{
				Header: map[string]string{"Authorization": "Bearer token"},
				Query:  map[string]string{"akey": "secret"},
			},
			Override: Override{
				Endpoint: upstream.URL + "?name=fromendpoint&ekey=eval",
			},
		}},
	})
	request := httptest.NewRequest(
		http.MethodPut,
		"/v1/images/generations?name=fromclient&ckey=cval&akey=attacker",
		strings.NewReader(`{"prompt":"x"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusNoContent)
	}
	got := <-captured
	if got.method != http.MethodPut {
		t.Fatalf("provider method = %q, want PUT", got.method)
	}
	wantURI := "/v1/images/generations?akey=secret&ckey=cval&ekey=eval&name=fromclient"
	if got.uri != wantURI {
		t.Fatalf("provider URI = %q, want %q", got.uri, wantURI)
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
		MaxRetries:       new(1),
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
		MaxRetries:       new(1),
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
		MaxRetries:       new(1),
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
		MaxRetries:       new(1),
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

func TestHandlerProgressingStreamKeepsSelectedInstanceHealthy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := range 8 {
			chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"tok" + strconv.Itoa(i) + "\"}}]}\n\n"
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Timeout: 30,
		Instances: []Instance{{
			Name: "progressing", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "messages":[{"role":"user","content":"hello"}],"stream":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for streaming request")
	})).ServeHTTP(rr, req)

	output := rr.Body.String()
	for i := range 8 {
		if !strings.Contains(output, "tok"+strconv.Itoa(i)) {
			t.Fatalf("progressing stream lost chunk %d; body = %q", i, output)
		}
	}
	if !p.instanceHealthy(0) {
		t.Fatal("selected instance marked unhealthy after progressing stream")
	}
}

func TestHandlerStalledStreamTimesOutForSelectedInstance(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"))
		flusher.Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Timeout: 30,
		Instances: []Instance{{
			Name: "stalled", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "messages":[{"role":"user","content":"hello"}],"stream":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	started := time.Now()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for streaming request")
	})).ServeHTTP(rr, req)

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled stream took %s, want progress timeout to abort promptly", elapsed)
	}
	if !strings.Contains(rr.Body.String(), "first") {
		t.Fatalf("stalled stream = %q, want first chunk before abort", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "late") {
		t.Fatalf("stalled stream = %q, want abort before the late chunk", rr.Body.String())
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
		MaxRetries:       new(1),
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

	// Probes run on the plugin-owned refresher, so the request itself must
	// not be the probe trigger: wake a refresh and wait for the published
	// snapshot to record the unhealthy instance before selecting.
	p.refreshHealth(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for p.instanceHealthy(0) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.instanceHealthy(0) {
		t.Fatal("health snapshot never recorded the failing probe")
	}

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

func TestHandlerPreservesRawBodyWhenProviderRequestIsUnchanged(t *testing.T) {
	const raw = `{ "messages" : [ { "role" : "user", "content" : "hello" } ], "top_p" : 0.9 }`
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		upstreamBody = string(body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Instances: []Instance{{
			Name:     "one",
			Provider: "openai-compatible",
			Weight:   1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
			Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		}},
	})

	serveChatWithBody(t, p, raw)

	if upstreamBody != raw {
		t.Fatalf("upstream body = %q, want exact raw body %q", upstreamBody, raw)
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
					RequestBodyForceOverride: new(true),
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
	const upstreamResponse = `{
		  "model": "gpt-4-0613",
		  "usage": {"prompt_tokens": 23, "completion_tokens": 8, "total_tokens": 31}
		}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamResponse))
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
	upstreamAddress := strings.TrimPrefix(upstream.URL, "http://")
	upstreamHost, _, _ := strings.Cut(upstreamAddress, ":")
	assertLLMRequestVar(t, req, "$upstream_addr", upstreamAddress)
	assertLLMRequestVar(t, req, "$upstream_status", "200")
	assertLLMRequestVar(t, req, "$upstream_uri", "/v1/chat/completions")
	assertLLMRequestVar(t, req, "$upstream_host", upstreamHost)
	assertLLMRequestVar(t, req, "$upstream_response_length", int64(len(upstreamResponse)))
	if got := apisixctx.GetRequestVar(req, "$upstream_response_time"); got == nil || got == "" {
		t.Fatal("$upstream_response_time is empty")
	}
}

func TestHandlerUpstreamResponseTimeExcludesAuthenticationAndIncludesResponseBody(t *testing.T) {
	const (
		authenticationDelay = 180 * time.Millisecond
		responseBodyDelay   = 80 * time.Millisecond
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(responseBodyDelay)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{{
			Name:     "one",
			Provider: "vertex-ai",
			Weight:   1,
			Auth:     Auth{GCP: &ai_auth.GCPConfig{ServiceAccountJSON: "test"}},
			Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		}},
	})
	p.gcpTokens = delayedGCPTokenApplier{delay: authenticationDelay}
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"ping"}]}`),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	got, ok := apisixctx.GetRequestVar(req, "$upstream_response_time").(string)
	if !ok {
		t.Fatalf("$upstream_response_time = %#v, want seconds string", got)
	}
	elapsed, err := time.ParseDuration(got + "s")
	if err != nil {
		t.Fatalf("parse $upstream_response_time %q: %v", got, err)
	}
	if elapsed < responseBodyDelay-20*time.Millisecond {
		t.Fatalf("$upstream_response_time = %v, want response body delay included", elapsed)
	}
	if elapsed >= authenticationDelay {
		t.Fatalf("$upstream_response_time = %v, want authentication delay excluded", elapsed)
	}
}

func TestHandlerLeavesUpstreamResponseVarsUnsetWhenProviderRequestFails(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstreamAddress := strings.TrimPrefix(upstream.URL, "http://")
	upstreamHost, _, _ := strings.Cut(upstreamAddress, ":")
	upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{{
			Name:     "one",
			Provider: "openai-compatible",
			Weight:   1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
			Options:  map[string]any{"model": "gpt-4"},
			Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		}},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"messages":[{"role":"user","content":"ping"}]}`),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want 503", rr.Code)
	}
	assertLLMRequestVar(t, req, "$upstream_addr", upstreamAddress)
	assertLLMRequestVar(t, req, "$upstream_uri", "/v1/chat/completions")
	assertLLMRequestVar(t, req, "$upstream_host", upstreamHost)
	if got := apisixctx.GetRequestVar(req, "$upstream_response_time"); got == nil || got == "" {
		t.Fatal("$upstream_response_time is empty")
	}
	for _, key := range []string{"$upstream_status", "$upstream_response_length"} {
		if got := apisixctx.GetRequestVar(req, key); got != nil {
			t.Fatalf("%s = %#v, want unset", key, got)
		}
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

func TestAppendBedrockEndpointEscapesARNModelAsOnePathSegment(t *testing.T) {
	const model = "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/test123"
	got, err := ai_protocols.AppendBedrockEndpoint("https://bedrock-runtime.us-east-1.amazonaws.com", model, false)
	if err != nil {
		t.Fatalf("ai_protocols.AppendBedrockEndpoint() error = %v", err)
	}
	const want = "https://bedrock-runtime.us-east-1.amazonaws.com/model/" +
		"arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Aapplication-inference-profile%2Ftest123/converse"
	if got != want {
		t.Fatalf("ai_protocols.AppendBedrockEndpoint() = %q, want %q", got, want)
	}
}

func TestHandlerRejectsInvalidBedrockInstanceRequestBeforeUpstream(t *testing.T) {
	tests := []struct {
		name       string
		options    map[string]any
		path       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "missing model",
			path:       "/ai/body-model-only/converse",
			body:       `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "could not resolve upstream path",
		},
		{
			name:       "non Converse protocol",
			options:    map[string]any{"model": "claude"},
			path:       "/ai/bedrock-chat",
			body:       `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "does not support openai-chat protocol",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(upstream.Close)
			p := newTestPlugin(t, Config{Instances: []Instance{{
				Name:         "bedrock",
				Provider:     "bedrock",
				ProviderConf: map[string]any{"region": "us-east-1"},
				Weight:       1,
				Auth: Auth{AWS: &ai_auth.AWSConfig{
					AccessKeyID: "key", SecretAccessKey: "secret",
				}},
				Options:  test.options,
				Override: Override{Endpoint: upstream.URL},
			}}})
			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next handler called for invalid Bedrock request")
			})).ServeHTTP(rr, req)

			if rr.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d; body = %q", rr.Code, test.wantStatus, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), test.wantBody) {
				t.Fatalf("response body = %q, want %q", rr.Body.String(), test.wantBody)
			}
			if got := upstreamCalls.Load(); got != 0 {
				t.Fatalf("upstream calls = %d, want 0", got)
			}
		})
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

	if rr.Body.String() != string(metadata) || !rr.Flushed {
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

type delayedGCPTokenApplier struct {
	delay time.Duration
}

func (a delayedGCPTokenApplier) Apply(
	_ context.Context,
	_ *http.Client,
	_ *http.Request,
	_ ai_auth.GCPConfig,
) error {
	time.Sleep(a.delay)
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

type countingRoundTripper struct {
	requests atomic.Int32
	closed   atomic.Int32
	base     http.RoundTripper
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.requests.Add(1)
	return c.base.RoundTrip(req)
}

func (c *countingRoundTripper) CloseIdleConnections() {
	c.closed.Add(1)
	if closeable, ok := c.base.(interface{ CloseIdleConnections() }); ok {
		closeable.CloseIdleConnections()
	}
}

func healthProbeConfig(endpoint string) Config {
	return Config{Instances: []Instance{
		{
			Name: "probe", Provider: "openai-compatible", Priority: 0, Weight: 1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer probe"}},
			Override: Override{Endpoint: endpoint},
			Checks: &HealthChecks{Active: ActiveHealthCheck{
				Type:     "http",
				HTTPPath: "/health",
				Timeout:  1,
			}},
		},
	}}
}

func TestHealthProbeReusesClientForRepeatedProbes(t *testing.T) {
	var probeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, healthProbeConfig(server.URL))
	client := p.healthClients[0]
	if client == nil {
		t.Fatal("no health-check client was built for the configured instance")
	}
	counting := &countingRoundTripper{base: client.Transport}
	client.Transport = counting

	ctx := context.Background()
	for range 3 {
		result := p.probeInstance(ctx, 0)
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("probe = %+v, want status 200", result)
		}
	}
	if p.healthClients[0] != client {
		t.Fatal("health-check client was rebuilt between probes")
	}
	if got := counting.requests.Load(); got != 3 {
		t.Fatalf("transport requests across three probes = %d, want 3 through the shared client", got)
	}
}

func TestHealthStopClosesIdleConnectionsOnConfigReplacement(t *testing.T) {
	var probeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	first := newTestPlugin(t, healthProbeConfig(server.URL))
	counting := &countingRoundTripper{base: first.healthClients[0].Transport}
	first.healthClients[0].Transport = counting
	_ = first.probeInstance(context.Background(), 0)

	// Configuration replacement stops the old plugin instance and closes the
	// idle connections of its probe client.
	first.Stop()
	if got := counting.closed.Load(); got != 1 {
		t.Fatalf("probe transport idle-connection closes = %d, want 1 on Stop", got)
	}

	replacement := newTestPlugin(t, healthProbeConfig(server.URL))
	if replacement.healthClients[0] == first.healthClients[0] {
		t.Fatal("replacement configuration reused the closed probe client")
	}
	result := replacement.probeInstance(context.Background(), 0)
	if result.err != nil || result.status != http.StatusOK {
		t.Fatalf("replacement probe = %+v, want status 200", result)
	}
}

func TestPostInitOwnsHealthLoopLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, healthProbeConfig(server.URL))
	if p.wakeHealth == nil {
		t.Fatal("wakeHealth not initialized after PostInit")
	}
	if p.stopHealth == nil {
		t.Fatal("stopHealth not initialized after PostInit")
	}
	if p.healthDone == nil {
		t.Fatal("healthDone not initialized after PostInit")
	}
	if p.healthCancel == nil {
		t.Fatal("healthCancel not initialized after PostInit")
	}
}

func TestStopHealthConcurrentWithWakes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, healthProbeConfig(server.URL))

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			p.wakeHealthRefresh()
		})
	}
	p.Stop()
	wg.Wait()

	select {
	case <-p.healthDone:
	case <-time.After(time.Second):
		t.Fatal("healthDone did not close after concurrent Stop")
	}
}

func TestStopHealthImmediatelyAfterPostInit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, healthProbeConfig(server.URL))
	p.Stop()
	p.Stop()

	select {
	case <-p.healthDone:
	case <-time.After(time.Second):
		t.Fatal("healthDone did not close after immediate Stop")
	}
}

func TestHealthProbeHonorsConfiguredTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	config := healthProbeConfig(server.URL)
	config.Instances[0].Checks.Active.Timeout = 0.05
	p := newTestPlugin(t, config)

	start := time.Now()
	result := p.probeInstance(context.Background(), 0)
	elapsed := time.Since(start)
	if !result.timeout {
		t.Fatalf("probe result = %+v, want a timeout", result)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("probe elapsed = %s, want the configured timeout to bound the probe", elapsed)
	}
}

func TestHealthProbeConcurrentProbesAreRaceFree(t *testing.T) {
	var probeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, healthProbeConfig(server.URL))
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			result := p.probeInstance(ctx, 0)
			if result.err != nil || result.status != http.StatusOK {
				t.Errorf("concurrent probe = %+v, want status 200", result)
			}
		})
	}
	wg.Wait()
}

func TestHealthDueCheckDoesNotDelaySelection(t *testing.T) {
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probeCalls.Add(1)
		close(probeStarted)
		<-releaseProbe
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, healthProbeConfig(server.URL))
	blocked := time.Now().Add(time.Hour)
	p.health[0].nextCheck = blocked.Add(-time.Hour)

	p.refreshHealth(context.Background())
	<-probeStarted

	// While the blocking probe is in flight, selection must use the last
	// snapshot and must not wait for the probe to finish.
	start := time.Now()
	index, ok := p.pickInstance(nil, nil)
	if !ok || index != 0 {
		t.Fatalf("pickInstance = (%d, %v), want instance 0", index, ok)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("selection waited %s for the in-flight probe", elapsed)
	}
	if !p.instanceHealthy(0) {
		t.Fatal("stale-but-valid health became unreadable during refresh")
	}

	close(releaseProbe)
	p.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for probeCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe calls = %d, want exactly 1", probeCalls.Load())
	}
}

func TestHealthConcurrentWakesRunOnlyOneRefreshPass(t *testing.T) {
	var probeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probeCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, healthProbeConfig(server.URL))
	p.health[0].nextCheck = time.Now().Add(-time.Second)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			p.refreshHealth(context.Background())
		})
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for probeCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := probeCalls.Load(); got != 1 {
		t.Fatalf("probe calls after 16 concurrent wakes = %d, want exactly 1", got)
	}
	p.Stop()
}

func TestHealthStopJoinsInFlightRefresh(t *testing.T) {
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probeCalls.Add(1)
		close(probeStarted)
		<-releaseProbe
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, healthProbeConfig(server.URL))
	p.health[0].nextCheck = time.Now().Add(-time.Second)
	p.refreshHealth(context.Background())
	<-probeStarted

	// Stop must join the refresher: it cannot return while the in-flight
	// probe is still blocked, and no probe can start afterwards.
	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the probe was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseProbe)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not join the refresher after the probe completed")
	}
	p.Stop()
	if got := probeCalls.Load(); got != 1 {
		t.Fatalf("probe calls after Stop = %d, want exactly 1", got)
	}
}

func TestReadJSONDocumentClassifiesOversizedBodyByTypeNotText(t *testing.T) {
	p := &Plugin{config: Config{MaxReqBodySize: 4}}
	tests := []struct {
		name     string
		request  *http.Request
		wantSize bool
	}{
		{
			name: "declared oversized",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
				req.ContentLength = 17
				return req
			}(),
			wantSize: true,
		},
		{
			name: "streamed oversized",
			request: func() *http.Request {
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/chat/completions",
					strings.NewReader(`{"model":"too-large"}`),
				)
				req.ContentLength = -1
				return req
			}(),
			wantSize: true,
		},
		{
			name: "invalid json",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{`))
				req.ContentLength = -1
				return req
			}(),
		},
		{
			name: "empty body",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(" \n"))
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := p.readJSONDocument(test.request)
			if err == nil {
				t.Fatal("readJSONDocument() error = nil")
			}
			if got := errors.Is(err, errRequestBodyTooLarge); got != test.wantSize {
				t.Fatalf("errors.Is(err, errRequestBodyTooLarge) = %v, want %v; error = %v", got, test.wantSize, err)
			}
			if test.wantSize && !strings.Contains(err.Error(), "max_req_body_size") {
				t.Fatalf("error text = %q, want size hint", err.Error())
			}
		})
	}

	// The handler maps the typed error to 413, never matching error text.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.ContentLength = 17
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler called for oversized request")
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("handler status = %d, want 413", rr.Code)
	}
}

func TestProviderBodyCopiesOnlyMutatedRequestFields(t *testing.T) {
	p := newTestPlugin(t, Config{Instances: []Instance{{Name: "one", Weight: 1}}})

	document := ai_protocols.Document{Raw: map[string]any{
		"model":    "caller-model",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}}
	instance := Instance{Name: "one", Provider: "openai-compatible", Options: map[string]any{"model": "gpt-4"}}

	_, providerDocument, err := p.providerBody(nil, document, ai_protocols.OpenAIChat, instance)
	if err != nil {
		t.Fatalf("providerBody() error = %v", err)
	}

	if got := providerDocument.Raw["model"]; got != "gpt-4" {
		t.Fatalf("provider model = %v, want gpt-4", got)
	}
	// The shared client document must never be mutated by the provider copy.
	if got := document.Raw["model"]; got != "caller-model" {
		t.Fatalf("client model = %v, want caller-model; client document mutated", got)
	}
}

func TestProviderBodyUnchangedReturnsOriginalBodyBytes(t *testing.T) {
	p := newTestPlugin(t, Config{Instances: []Instance{{Name: "one", Weight: 1}}})

	const raw = `{"model":"caller-model","messages":[{"role":"user","content":"hello"}]}`
	document := ai_protocols.Document{Raw: map[string]any{
		"model":    "caller-model",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}}
	instance := Instance{Name: "one", Provider: "openai-compatible"}

	body, providerDocument, err := p.providerBody([]byte(raw), document, ai_protocols.OpenAIChat, instance)
	if err != nil {
		t.Fatalf("providerBody() error = %v", err)
	}
	if string(body) != raw {
		t.Fatalf("provider body = %q, want exact raw bytes", body)
	}
	if got := providerDocument.Raw["model"]; got != "caller-model" {
		t.Fatalf("provider model = %v, want caller-model", got)
	}
	if got := document.Raw["model"]; got != "caller-model" {
		t.Fatalf("client model = %v, want caller-model", got)
	}
}

func TestWeightSelectionHonorsConfiguredWeightsWithoutExpansion(t *testing.T) {
	instances := make([]Instance, 0, 3)
	for i, weight := range []int{1, 3, 6} {
		instances = append(instances, Instance{
			Name:     "weighted-" + strconv.Itoa(i),
			Provider: "openai-compatible",
			Weight:   weight,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
			Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
		})
	}
	p := newTestPlugin(t, Config{Instances: instances})

	sel := p.selection[0]
	if sel == nil {
		t.Fatal("no weight selection index for priority 0")
	}
	// The index must stay compact: cumulative weights over distinct IDs, not
	// an expanded repeated-provider slice.
	if len(sel.ids) != 3 || len(sel.cumulative) != 3 {
		t.Fatalf(
			"selection index = %d ids, %d cumulative, want 3 each (no expansion)",
			len(sel.ids),
			len(sel.cumulative),
		)
	}
	if sel.total != 10 {
		t.Fatalf("total weight = %d, want 10", sel.total)
	}
	wantCumulative := []int{1, 4, 10}
	for i, want := range wantCumulative {
		if sel.cumulative[i] != want {
			t.Fatalf("cumulative[%d] = %d, want %d", i, sel.cumulative[i], want)
		}
	}

	// Property: every slot maps to the configured instance; the mapping is a
	// permutation of the distinct IDs in weight order.
	slotInstances := make(map[int]int)
	for slot := range sel.total {
		slotInstances[weightInstanceAtSlot(sel, slot)]++
	}
	wantSlots := []int{1, 3, 6}
	for i, want := range wantSlots {
		if slotInstances[i] != want {
			t.Fatalf("instance %d received %d slots, want weight %d", i, slotInstances[i], want)
		}
	}
}

func TestPickInstanceDistributesByWeightOverManyPicks(t *testing.T) {
	instances := make([]Instance, 0, 2)
	for i, weight := range []int{1, 3} {
		instances = append(instances, Instance{
			Name:     "weighted-" + strconv.Itoa(i),
			Provider: "openai-compatible",
			Weight:   weight,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
			Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
		})
	}
	p := newTestPlugin(t, Config{Instances: instances})

	const picks = 4000
	counts := make(map[int]int)
	for range picks {
		index, ok := p.pickInstance(nil, nil)
		if !ok {
			t.Fatal("pickInstance() = false, want an instance")
		}
		counts[index]++
	}
	// With weights 1:3 the heavier instance must receive ~3x the picks within
	// a generous tolerance; round-robin over the slot table enforces the exact
	// ratio over a full cycle.
	if counts[0] < picks/5 || counts[1] < 3*picks/5 {
		t.Fatalf("pick distribution = %#v over %d picks, want ~1:3", counts, picks)
	}
}

func TestInstanceIndexResolvesByNameAndMissingIDs(t *testing.T) {
	instances := []Instance{
		{
			Name:     "first",
			Provider: "openai-compatible",
			Weight:   1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
			Override: Override{Endpoint: "http://127.0.0.1/v1"},
		},
		{
			Name:     "second",
			Provider: "openai-compatible",
			Weight:   1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
			Override: Override{Endpoint: "http://127.0.0.1/v2"},
		},
	}
	p := newTestPlugin(t, Config{Instances: instances})

	if index, ok := p.instanceIndex("first"); !ok || index != 0 {
		t.Fatalf("instanceIndex(first) = (%d, %v), want (0, true)", index, ok)
	}
	if index, ok := p.instanceIndex("second"); !ok || index != 1 {
		t.Fatalf("instanceIndex(second) = (%d, %v), want (1, true)", index, ok)
	}
	if _, ok := p.instanceIndex("missing"); ok {
		t.Fatal("instanceIndex(missing) = ok, want false")
	}
	if _, ok := p.instanceIndex(""); ok {
		t.Fatal("instanceIndex(empty) = ok, want false")
	}
}

func TestInstanceIndexDuplicateNamesKeepFirstOccurrence(t *testing.T) {
	instances := []Instance{
		{
			Name:     "dup",
			Provider: "openai-compatible",
			Weight:   1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
			Override: Override{Endpoint: "http://127.0.0.1/v1"},
		},
		{
			Name:     "dup",
			Provider: "openai-compatible",
			Weight:   1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
			Override: Override{Endpoint: "http://127.0.0.1/v2"},
		},
	}
	p := newTestPlugin(t, Config{Instances: instances})

	index, ok := p.instanceIndex("dup")
	if !ok || index != 0 {
		t.Fatalf("instanceIndex(dup) = (%d, %v), want first occurrence (0, true)", index, ok)
	}
}
