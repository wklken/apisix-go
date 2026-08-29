package ai_proxy_multi

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	observabilitymetrics "github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	p, _ := newTestPluginWithTasks(t, cfg)
	return p
}

func newTestPluginWithTasks(t *testing.T, cfg Config) (*Plugin, *runtime.TaskRegistry) {
	t.Helper()

	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/ai-multi/attempt-1", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	p := &Plugin{
		config: cfg,
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
		},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	p.stoppedHealth.Store(true)
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if err := p.refreshResolvedNodes(context.Background(), true); err != nil {
		t.Fatalf("refreshResolvedNodes() error = %v", err)
	}
	p.publishHealthSnapshot()
	for index := range p.config.Instances {
		for _, node := range p.resolvedNodes(index) {
			node.client = &http.Client{Transport: multiStreamRoundTripFunc(
				func(request *http.Request) (*http.Response, error) {
					return p.client.Transport.RoundTrip(request)
				},
			)}
			node.healthClient = &http.Client{Transport: multiStreamRoundTripFunc(
				func(request *http.Request) (*http.Response, error) {
					client := p.healthClients[index]
					if client == nil {
						return nil, errors.New("test health client is unavailable")
					}
					return client.Transport.RoundTrip(request)
				},
			)}
		}
	}
	p.stoppedHealth.Store(false)
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})

	return p, tasks
}

func TestHashVariableUsesEffectiveRemoteIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	r = r.WithContext(context.WithValue(r.Context(), apisixctx.RemoteAddrKey, "198.51.100.2"))

	if got := hashVariable(r, "remote_addr"); got != "198.51.100.2" {
		t.Fatalf("hashVariable(remote_addr) = %q, want effective remote address", got)
	}
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

func TestSchemaMatchesAPISIX317BaseProviderAndEndpointContracts(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	valid := map[string]any{
		"instances": []any{map[string]any{
			"name": "openai-official", "provider": "openai", "weight": 1,
			"auth":    map[string]any{"header": map[string]any{"some_header": "some_value"}},
			"options": map[string]any{"model": "gpt-4"},
		}},
	}
	if err := util.Validate(valid, p.GetSchema()); err != nil {
		t.Fatalf("minimal APISIX 3.17 configuration rejected: %v", err)
	}

	unsupportedProvider := ai_common.CloneJSONValue(valid).(map[string]any)
	unsupportedProvider["instances"].([]any)[0].(map[string]any)["provider"] = "some-unique"
	if err := util.Validate(unsupportedProvider, p.GetSchema()); err == nil {
		t.Fatal("unsupported provider was accepted")
	}

	invalidEndpoint := ai_common.CloneJSONValue(valid).(map[string]any)
	invalidEndpoint["instances"].([]any)[0].(map[string]any)["override"] = map[string]any{
		"endpoint": "http//127.0.0.1:1980",
	}
	if err := util.Validate(invalidEndpoint, p.GetSchema()); err == nil {
		t.Fatal("invalid override endpoint was accepted")
	}
}

// TestSchemaRejectsAPISIX317MissingBedrockSecret mirrors
// t/plugin/ai-proxy-bedrock.t TEST 5.  The plugin schema owns the required
// AWS fields; route publication must not be used as a substitute for this
// direct contract test.
func TestSchemaRejectsAPISIX317MissingBedrockSecret(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"instances": []any{map[string]any{
			"name":     "bedrock-bad",
			"provider": "bedrock",
			"weight":   1,
			"auth": map[string]any{
				"aws": map[string]any{"access_key_id": "AKIAIOSFODNN7EXAMPLE"},
			},
			"provider_conf": map[string]any{"region": "us-east-1"},
			"options":       map[string]any{"model": "anthropic.claude-3-5-sonnet-20241022-v2:0"},
		}},
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("Bedrock instance without secret_access_key was accepted")
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

func TestHandlerMatchesAPISIX317EmptyRequestBodyResponse(t *testing.T) {
	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name:     "openai",
		Provider: "openai",
		Weight:   1,
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
	}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for empty request body")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	const wantBody = "{\"message\":\"could not get body: request body is empty\"}\n"
	if got := rr.Body.String(); got != wantBody {
		t.Fatalf("response body = %q, want %q", got, wantBody)
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
			time.Sleep(25 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Timeout: 100,
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
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Timeout: 100,
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

type blockingMultiRequestBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingMultiRequestBody() *blockingMultiRequestBody {
	return &blockingMultiRequestBody{closed: make(chan struct{})}
}

func (b *blockingMultiRequestBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, os.ErrDeadlineExceeded
}

func (b *blockingMultiRequestBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestTransportTimesOutStalledRequestBodySend(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{Timeout: 30, Instances: []Instance{{
			Name:     "one",
			Provider: "openai-compatible",
			Weight:   1,
			Override: Override{Endpoint: "http://example.com/v1/chat/completions"},
		}}},
	)
	transport := p.transport()
	request, err := http.NewRequest(http.MethodPost, "http://example.com", newBlockingMultiRequestBody())
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request = request.WithContext(context.Background())

	started := time.Now()
	_, err = transport.RoundTrip(request)
	if err == nil {
		t.Fatal("RoundTrip() error = nil, want timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, os.ErrDeadlineExceeded) {
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("RoundTrip() error = %v, want timeout", err)
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("RoundTrip() took %s, want send timeout to abort promptly", elapsed)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("RoundTrip() took %s, want timeout to be armed", elapsed)
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

func TestAPISIX317HealthTransitionsForOneAndTwoCheckedInstances(t *testing.T) {
	newProvider := func(t *testing.T, health *atomic.Int64, name string) *httptest.Server {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/health" {
				w.WriteHeader(int(health.Load()))
				return
			}
			_, _ = w.Write([]byte(`{"instance":"` + name + `"}`))
		}))
		t.Cleanup(server.Close)
		return server
	}
	checks := func() *HealthChecks {
		return &HealthChecks{Active: ActiveHealthCheck{
			HTTPPath: "/health", Host: "health.authority.test",
			Healthy: HealthyCheckPolicy{
				HTTPStatuses: []int{http.StatusOK}, Successes: 1,
			},
			Unhealthy: UnhealthyCheckPolicy{
				HTTPStatuses: []int{http.StatusInternalServerError}, HTTPFailures: 1,
			},
		}}
	}
	waitHealth := func(t *testing.T, p *Plugin, want ...bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			got := make([]bool, len(want))
			for index := range want {
				got[index] = p.instanceHealthy(index)
			}
			if slices.Equal(got, want) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		got := make([]bool, len(want))
		for index := range want {
			got[index] = p.instanceHealthy(index)
		}
		t.Fatalf("instance health = %v, want %v", got, want)
	}
	installClock := func(p *Plugin) *atomic.Int64 {
		clock := &atomic.Int64{}
		clock.Store(time.Now().UnixNano())
		p.healthNow = func() time.Time { return time.Unix(0, clock.Load()) }
		return clock
	}
	refresh := func(t *testing.T, p *Plugin, clock *atomic.Int64, want ...bool) {
		t.Helper()
		clock.Add(int64(2 * time.Second))
		p.refreshHealth(context.Background())
		waitHealth(t, p, want...)
	}

	t.Run("one checked instance recovers into balanced selection", func(t *testing.T) {
		var checkedHealth atomic.Int64
		checkedHealth.Store(http.StatusInternalServerError)
		checked := newProvider(t, &checkedHealth, "checked")
		var uncheckedHealth atomic.Int64
		uncheckedHealth.Store(http.StatusOK)
		unchecked := newProvider(t, &uncheckedHealth, "unchecked")

		p := newTestPlugin(t, Config{Instances: []Instance{
			{
				Name: "checked", Provider: "openai-compatible", Weight: 1,
				Override: Override{Endpoint: checked.URL + "/v1/chat/completions"}, Checks: checks(),
			},
			{
				Name: "unchecked", Provider: "openai-compatible", Weight: 1,
				Override: Override{Endpoint: unchecked.URL + "/v1/chat/completions"},
			},
		}})
		clock := installClock(p)

		refresh(t, p, clock, false, true)
		for range 4 {
			if body := serveChat(t, p, ""); !strings.Contains(body, `"instance":"unchecked"`) {
				t.Fatalf("unhealthy selection body = %q, want unchecked instance", body)
			}
		}

		checkedHealth.Store(http.StatusOK)
		refresh(t, p, clock, true, true)
		counts := map[string]int{}
		for range 10 {
			body := serveChat(t, p, "")
			switch {
			case strings.Contains(body, `"instance":"checked"`):
				counts["checked"]++
			case strings.Contains(body, `"instance":"unchecked"`):
				counts["unchecked"]++
			default:
				t.Fatalf("unexpected provider body %q", body)
			}
		}
		if diff := counts["checked"] - counts["unchecked"]; diff < -2 || diff > 2 {
			t.Fatalf("recovered selection = %#v, want distribution difference <= 2", counts)
		}
	})

	t.Run("two checked instances exchange health", func(t *testing.T) {
		var firstHealth atomic.Int64
		firstHealth.Store(http.StatusInternalServerError)
		first := newProvider(t, &firstHealth, "first")
		var secondHealth atomic.Int64
		secondHealth.Store(http.StatusOK)
		second := newProvider(t, &secondHealth, "second")

		p := newTestPlugin(t, Config{Instances: []Instance{
			{
				Name: "first", Provider: "openai-compatible", Weight: 1,
				Override: Override{Endpoint: first.URL + "/v1/chat/completions"}, Checks: checks(),
			},
			{
				Name: "second", Provider: "openai-compatible", Weight: 1,
				Override: Override{Endpoint: second.URL + "/v1/chat/completions"}, Checks: checks(),
			},
		}})
		clock := installClock(p)

		refresh(t, p, clock, false, true)
		for range 4 {
			if body := serveChat(t, p, ""); !strings.Contains(body, `"instance":"second"`) {
				t.Fatalf("first transition body = %q, want second instance", body)
			}
		}

		firstHealth.Store(http.StatusOK)
		secondHealth.Store(http.StatusInternalServerError)
		refresh(t, p, clock, true, false)
		for range 4 {
			if body := serveChat(t, p, ""); !strings.Contains(body, `"instance":"first"`) {
				t.Fatalf("second transition body = %q, want first instance", body)
			}
		}
	})
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
				Auth: Auth{
					Header: map[string]string{"Authorization": "Bearer test-token"},
					Query:  map[string]string{"api-key": "query-secret"},
				},
				Options:  map[string]any{"model": "gpt-4"},
				Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "model": "client-model",
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
	assertLLMRequestVar(t, req, "$request_llm_model", "client-model")
	assertLLMRequestVar(t, req, "$llm_model", "gpt-4-0613")
	assertLLMRequestVar(t, req, "$balancer_ip", "one")
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
	assertLLMRequestVar(t, req, "$upstream_uri", "/v1/chat/completions?api-key=query-secret")
	if got := apisixlog.GetField(req, "$upstream_uri"); got != "/v1/chat/completions?api-key=***" {
		t.Fatalf("logged upstream URI = %#v, want auth query redacted", got)
	}
	assertLLMRequestVar(t, req, "$upstream_host", upstreamHost)
	assertLLMRequestVar(t, req, "$upstream_response_length", int64(len(upstreamResponse)))
	if got := apisixctx.GetRequestVar(req, "$upstream_response_time"); got == nil || got == "" {
		t.Fatal("$upstream_response_time is empty")
	}
	if got := apisixctx.GetRequestVar(req, "$llm_time_to_first_token"); got == nil {
		t.Fatal("$llm_time_to_first_token is unset")
	}
}

func TestHandlerTracksActiveLLMConnection(t *testing.T) {
	previous := observabilitymetrics.LLMActiveConnections
	observabilitymetrics.LLMActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_ai_proxy_multi_active"},
		[]string{
			"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
			"request_type", "request_llm_model", "llm_model",
		},
	)
	t.Cleanup(func() { observabilitymetrics.LLMActiveConnections = previous })

	activeGauge := func(node, requestModel, providerModel string) prometheus.Gauge {
		return observabilitymetrics.LLMActiveConnections.WithLabelValues(
			"", "", "", "", "", "", "", node, "ai_chat", requestModel, providerModel,
		)
	}
	runSingleAttempt := func(t *testing.T, response *http.Response, responseErr error) {
		t.Helper()
		entered := make(chan struct{})
		release := make(chan struct{})
		p := newTestPlugin(t, Config{Instances: []Instance{{
			Name: "provider-node", Provider: "openai-compatible", Weight: 1,
			Options:  map[string]any{"model": "provider-model"},
			Override: Override{Endpoint: "http://provider.test/v1/chat/completions"},
		}}})
		p.client.Transport = multiStreamRoundTripFunc(func(*http.Request) (*http.Response, error) {
			close(entered)
			<-release
			return response, responseErr
		})
		req := apisixctx.WithRequestVars(httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hello"}]}`),
		))
		req.Header.Set("Content-Type", "application/json")
		done := make(chan struct{})
		go func() {
			p.Handler(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), req)
			close(done)
		}()
		<-entered
		gauge := activeGauge("provider-node", "client-model", "provider-model")
		if got := testMultiGaugeValue(t, gauge); got != 1 {
			t.Fatalf("active connections = %v, want 1", got)
		}
		close(release)
		<-done
		if got := testMultiGaugeValue(t, gauge); got != 0 {
			t.Fatalf("active connections after terminal path = %v, want 0", got)
		}
	}

	t.Run("normal", func(t *testing.T) {
		runSingleAttempt(t, &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"model":"response-model","choices":[{"message":{"content":"ok"}}]}`,
			)),
		}, nil)
	})
	t.Run("error", func(t *testing.T) {
		runSingleAttempt(t, nil, errors.New("provider unavailable"))
	})
	t.Run("retry", func(t *testing.T) {
		enteredFirst := make(chan struct{})
		releaseFirst := make(chan struct{})
		enteredSecond := make(chan struct{})
		releaseSecond := make(chan struct{})
		p := newTestPlugin(t, Config{
			FallbackStrategy: "http_5xx",
			MaxRetries:       new(1),
			Instances: []Instance{
				{
					Name: "first", Provider: "openai-compatible", Weight: 1,
					Options:  map[string]any{"model": "provider-one"},
					Override: Override{Endpoint: "http://first.test/v1/chat/completions"},
				},
				{
					Name: "second", Provider: "openai-compatible", Weight: 1,
					Options:  map[string]any{"model": "provider-two"},
					Override: Override{Endpoint: "http://second.test/v1/chat/completions"},
				},
			},
		})
		var calls atomic.Int64
		p.client.Transport = multiStreamRoundTripFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				close(enteredFirst)
				<-releaseFirst
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
				}, nil
			}
			close(enteredSecond)
			<-releaseSecond
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"model":"response-model","choices":[{"message":{"content":"ok"}}]}`,
				)),
			}, nil
		})
		req := apisixctx.WithRequestVars(httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hello"}]}`),
		))
		req.Header.Set("Content-Type", "application/json")
		done := make(chan struct{})
		go func() {
			p.Handler(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), req)
			close(done)
		}()

		<-enteredFirst
		firstGauge := activeGauge("first", "client-model", "provider-one")
		if got := testMultiGaugeValue(t, firstGauge); got != 1 {
			t.Fatalf("first attempt active connections = %v, want 1", got)
		}
		close(releaseFirst)
		<-enteredSecond
		if got := testMultiGaugeValue(t, firstGauge); got != 0 {
			t.Fatalf("first attempt active connections after retry = %v, want 0", got)
		}
		secondGauge := activeGauge("second", "client-model", "provider-two")
		if got := testMultiGaugeValue(t, secondGauge); got != 1 {
			t.Fatalf("second attempt active connections = %v, want 1", got)
		}
		close(releaseSecond)
		<-done
		if got := testMultiGaugeValue(t, secondGauge); got != 0 {
			t.Fatalf("second attempt active connections after terminal response = %v, want 0", got)
		}
	})
	t.Run("retry clears stale model on error", func(t *testing.T) {
		enteredSecond := make(chan struct{})
		releaseSecond := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseSecond) }) }
		t.Cleanup(release)
		p := newTestPlugin(t, Config{
			FallbackStrategy: "http_5xx",
			MaxRetries:       new(1),
			Instances: []Instance{
				{
					Name: "first", Provider: "openai-compatible", Weight: 1,
					Options:  map[string]any{"model": "provider-one"},
					Override: Override{Endpoint: "http://first.test/v1/chat/completions"},
				},
				{
					Name: "second", Provider: "openai-compatible", Weight: 1,
					Override: Override{Endpoint: "http://second.test/v1/chat/completions"},
				},
			},
		})
		var calls atomic.Int64
		p.client.Transport = multiStreamRoundTripFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
				}, nil
			}
			close(enteredSecond)
			<-releaseSecond
			return nil, errors.New("second provider unavailable")
		})
		req := apisixctx.WithRequestVars(httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
		))
		req.Header.Set("Content-Type", "application/json")
		done := make(chan struct{})
		go func() {
			p.Handler(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), req)
			close(done)
		}()

		<-enteredSecond
		secondGauge := activeGauge("second", "", "")
		if got := testMultiGaugeValue(t, secondGauge); got != 1 {
			t.Fatalf("model-less second attempt active connections = %v, want 1", got)
		}
		if got := testMultiGaugeValue(t, activeGauge("second", "", "provider-one")); got != 0 {
			t.Fatalf("stale first-model active connections = %v, want 0", got)
		}
		release()
		<-done
		if got := testMultiGaugeValue(t, secondGauge); got != 0 {
			t.Fatalf("model-less second attempt after terminal error = %v, want 0", got)
		}
		if got := apisixctx.GetRequestVar(req, "$request_llm_model"); got != nil {
			t.Fatalf("$request_llm_model = %#v, want omitted client model preserved", got)
		}
		if got := apisixctx.GetRequestVar(req, "$llm_model"); got != nil {
			t.Fatalf("$llm_model = %#v, want cleared after model-less retry", got)
		}
	})
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
	if got := apisixctx.GetRequestVar(req, "$request_llm_model"); got != nil {
		t.Fatalf("$request_llm_model = %#v, want unset when the client omits model", got)
	}
	assertLLMRequestVar(t, req, "$llm_model", "text-embedding-005")
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
	assertLLMRequestVar(t, req, "$ai_stream_outcome", string(ai_stream.StreamOutcomeSuccess))
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

func testMultiGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
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

func newAIHealthTestTasks(t *testing.T) (*runtime.TaskRegistry, *runtime.TaskOwner, <-chan runtime.TaskFailure) {
	t.Helper()
	failures := make(chan runtime.TaskFailure, 8)
	tasks := runtime.NewTaskRegistry(context.Background(), func(failure runtime.TaskFailure) {
		failures <- failure
	})
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/ai-multi/attempt-1", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	return tasks, owner, failures
}

func newAIHealthPlugin(t *testing.T, tasks *runtime.TaskRegistry, owner *runtime.TaskOwner, cfg Config) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	p.stoppedHealth.Store(true)
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	p.stoppedHealth.Store(false)
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	return p
}

func newBlockingHealthPlugin(
	t *testing.T,
	tasks *runtime.TaskRegistry,
	owner *runtime.TaskOwner,
) (*Plugin, <-chan struct{}, <-chan struct{}, func()) {
	t.Helper()
	p := newAIHealthPlugin(t, tasks, owner, healthProbeConfig("http://192.0.2.10"))
	p.health[0].nextCheck = time.Now().Add(-time.Second)
	probeStarted := make(chan struct{})
	probeCanceled := make(chan struct{})
	release := make(chan struct{})
	var startOnce, cancelOnce, releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseProbe)
	p.probeForTest = func(ctx context.Context, index int) healthProbeResult {
		if index != 0 {
			return healthyProbeResult(index)
		}
		startOnce.Do(func() { close(probeStarted) })
		select {
		case <-ctx.Done():
			cancelOnce.Do(func() { close(probeCanceled) })
			<-release
		case <-release:
		}
		return healthyProbeResult(index)
	}
	return p, probeStarted, probeCanceled, releaseProbe
}

func newTwoProbeHealthPlugin(t *testing.T, tasks *runtime.TaskRegistry, owner *runtime.TaskOwner) *Plugin {
	t.Helper()
	cfg := healthProbeConfig("http://192.0.2.10")
	second := cfg.Instances[0]
	second.Name = "probe-2"
	cfg.Instances = append(cfg.Instances, second)
	p := newAIHealthPlugin(t, tasks, owner, cfg)
	for _, state := range p.health {
		state.nextCheck = time.Now().Add(-time.Second)
	}
	return p
}

func healthyProbeResult(_ int) healthProbeResult {
	return healthProbeResult{status: http.StatusOK}
}

func stopTestRegistry(t *testing.T, tasks *runtime.TaskRegistry) {
	t.Helper()
	residuals, err := tasks.Stop(context.Background())
	if err != nil || len(residuals) != 0 {
		t.Errorf("TaskRegistry.Stop() = (%v, %v)", residuals, err)
	}
}

func awaitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for completion")
	}
}

func assertNotClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("operation completed before the admitted probe was released")
	default:
	}
}

func awaitOwnerExit(t *testing.T, tasks *runtime.TaskRegistry, owner string) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		active := tasks.Active()
		if !slices.Contains(active, owner) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("owner %q did not exit; active = %v", owner, active)
		case <-ticker.C:
		}
	}
}

func TestAIHealthUsesAttemptQualifiedTaskOwners(t *testing.T) {
	tasks, owner, _ := newAIHealthTestTasks(t)
	p, probeStarted, _, release := newBlockingHealthPlugin(t, tasks, owner)
	p.wakeHealthRefresh()
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("health probe did not start")
	}

	got := tasks.Active()
	want := []string{
		"plugin/test/ai-multi/attempt-1/health-probe-worker-0",
		"plugin/test/ai-multi/attempt-1/health-refresh",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("active task owners = %v, want %v", got, want)
	}
	release()
	stopTestRegistry(t, tasks)
	p.Stop()
}

func TestAIHealthGenerationStopJoinsOwnedInFlightProbes(t *testing.T) {
	tasks, owner, _ := newAIHealthTestTasks(t)
	p, probeStarted, probeCanceled, release := newBlockingHealthPlugin(t, tasks, owner)
	p.wakeHealthRefresh()
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("health probe did not start")
	}
	stopped := make(chan struct{})
	go func() {
		stopTestRegistry(t, tasks)
		p.Stop()
		close(stopped)
	}()
	select {
	case <-probeCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("health probe did not observe generation cancellation")
	}
	assertNotClosed(t, stopped)
	release()
	awaitClosed(t, stopped)
}

func TestAIHealthProbePanicDoesNotFailOwnedLoop(t *testing.T) {
	tasks, owner, failures := newAIHealthTestTasks(t)
	p := newTwoProbeHealthPlugin(t, tasks, owner)
	recoveredCycle := make(chan struct{})
	var firstCalls atomic.Int32
	p.probeForTest = func(_ context.Context, index int) healthProbeResult {
		if index == 0 {
			if firstCalls.Add(1) == 1 {
				panic("probe-panic")
			}
			select {
			case <-recoveredCycle:
			default:
				close(recoveredCycle)
			}
		}
		return healthyProbeResult(index)
	}
	p.wakeHealthRefresh()
	select {
	case <-recoveredCycle:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("health loop did not run another cycle after a probe panic")
	}
	select {
	case failure := <-failures:
		t.Fatalf("recovered probe panic failed the generation task owner: %#v", failure)
	default:
	}
}

func TestAIHealthLoopProbesAgainAtIdleDeadline(t *testing.T) {
	tasks, owner, failures := newAIHealthTestTasks(t)
	p := newAIHealthPlugin(t, tasks, owner, healthProbeConfig("http://192.0.2.10"))
	p.health[0].nextCheck = time.Now().Add(-time.Second)
	probes := make(chan struct{}, 3)
	p.probeForTest = func(context.Context, int) healthProbeResult {
		probes <- struct{}{}
		return healthProbeResult{status: http.StatusOK}
	}

	p.wakeHealthRefresh()
	for probe := 1; probe <= 2; probe++ {
		select {
		case <-probes:
		case <-time.After(2 * time.Second):
			t.Fatalf("idle health probe %d did not run at its configured deadline", probe)
		}
	}
	select {
	case failure := <-failures:
		t.Fatalf("unexpected health task failure = %#v", failure)
	default:
	}
}

func TestPluginStopCancelsOwnedPeriodicHealthLoop(t *testing.T) {
	tasks, owner, _ := newAIHealthTestTasks(t)
	p := newAIHealthPlugin(t, tasks, owner, healthProbeConfig("http://192.0.2.10"))
	p.health[0].nextCheck = time.Now().Add(-time.Second)
	probed := make(chan struct{}, 1)
	p.probeForTest = func(context.Context, int) healthProbeResult {
		probed <- struct{}{}
		return healthProbeResult{status: http.StatusOK}
	}
	p.wakeHealthRefresh()
	select {
	case <-probed:
	case <-time.After(time.Second):
		t.Fatal("initial health probe did not run")
	}

	p.Stop()
	awaitOwnerExit(t, tasks, "plugin/test/ai-multi/attempt-1/health-refresh")
}

func TestProviderRequestNormalizesDefaultPortAuthority(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		wantHost string
	}{
		{name: "HTTP default", endpoint: "http://127.0.0.1:80", wantHost: "127.0.0.1"},
		{name: "HTTPS default", endpoint: "https://127.0.0.1:443", wantHost: "127.0.0.1"},
		{name: "HTTP non-default", endpoint: "http://127.0.0.1:8080", wantHost: "127.0.0.1:8080"},
		{name: "HTTPS non-default", endpoint: "https://127.0.0.1:8443", wantHost: "127.0.0.1:8443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Instances: []Instance{{
				Name: "provider", Provider: "openai-compatible", Weight: 1,
				Override: Override{Endpoint: test.endpoint},
			}}})
			var gotHost string
			p.client.Transport = multiStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				gotHost = request.URL.Host
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`{"choices":[{"message":{"content":"ok","role":"assistant"}}]}`,
					)),
					Request: request,
				}, nil
			})
			_ = serveChat(t, p, "")
			if gotHost != test.wantHost {
				t.Fatalf("provider URL Host = %q, want %q", gotHost, test.wantHost)
			}
		})
	}
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

func TestAPISIX317HealthDNSAddressChangesStayAttachedToInstance(t *testing.T) {
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(first.Close)
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(second.Close)

	p := newTestPlugin(t, healthProbeConfig("http://test.example.com"))
	var resolvedAddress atomic.Value
	resolvedAddress.Store(strings.TrimPrefix(first.URL, "http://"))
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, resolvedAddress.Load().(string))
	}}
	p.healthClients[0].Transport = transport

	firstResult := p.probeInstance(context.Background(), 0)
	if firstResult.err != nil || firstResult.status != http.StatusOK {
		t.Fatalf("first resolved address probe = %+v, want status 200", firstResult)
	}
	transport.CloseIdleConnections()
	resolvedAddress.Store(strings.TrimPrefix(second.URL, "http://"))
	secondResult := p.probeInstance(context.Background(), 0)
	if secondResult.err != nil || secondResult.status != http.StatusOK {
		t.Fatalf("changed resolved address probe = %+v, want status 200", secondResult)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("probe calls after address change = (%d, %d), want (1, 1)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestAPISIX317HealthCheckHostIsAuthorityNotDialTarget(t *testing.T) {
	var gotHost string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	config := healthProbeConfig(server.URL + "/v1/chat/completions")
	config.Instances[0].Checks.Active.Host = "health.authority.test"
	p := newTestPlugin(t, config)
	result := p.probeInstance(context.Background(), 0)
	if result.err != nil || result.status != http.StatusOK {
		t.Fatalf("health probe = %+v, want endpoint target status 200", result)
	}
	if gotHost != "health.authority.test" {
		t.Fatalf("health probe Host = %q, want configured authority", gotHost)
	}
}

func TestAPISIX317DNSOrderChangeDoesNotReplaceInFlightHealthProbe(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		close(firstStarted)
		<-releaseFirst
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(first.Close)
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(second.Close)

	p, tasks := newTestPluginWithTasks(t, healthProbeConfig("http://multi-ip.example.com"))
	client := p.healthClients[0]
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	p.healthNow = func() time.Time { return time.Unix(0, clock.Load()) }
	var resolvedAddress atomic.Value
	resolvedAddress.Store(strings.TrimPrefix(first.URL, "http://"))
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, resolvedAddress.Load().(string))
	}}
	client.Transport = transport
	p.refreshHealth(context.Background())
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first health probe did not start")
	}

	resolvedAddress.Store(strings.TrimPrefix(second.URL, "http://"))
	for range 16 {
		p.refreshHealth(context.Background())
	}
	if p.healthClients[0] != client {
		t.Fatal("DNS order change replaced the plugin-owned health client")
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("second address probes while first was in flight = %d, want 0", secondCalls.Load())
	}
	close(releaseFirst)
	awaitOwnerExit(t, tasks, "plugin/test/ai-multi/attempt-1/health-probe-worker-0")
	clock.Add(int64(2 * time.Second))
	deadline := time.Now().Add(2 * time.Second)
	transport.CloseIdleConnections()
	p.refreshHealth(context.Background())
	for secondCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 || !p.instanceHealthy(0) {
		t.Fatalf(
			"probes/health after DNS change = (%d, %d, %v), want (1, 1, true)",
			firstCalls.Load(), secondCalls.Load(), p.instanceHealthy(0),
		)
	}
}

func TestAPISIX317DomainHealthAndRequestUseResolvedAddresses(t *testing.T) {
	newProvider := func(t *testing.T, address string) *httptest.Server {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"host":"` + r.Host + `","server_addr":"` + address +
				`","choices":[{"message":{"content":"ok","role":"assistant"}}]}`))
		}))
		t.Cleanup(server.Close)
		return server
	}
	first := newProvider(t, "first")
	second := newProvider(t, "second")
	var resolvedAddress atomic.Value
	resolvedAddress.Store(strings.TrimPrefix(first.URL, "http://"))
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, resolvedAddress.Load().(string))
	}}
	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://multi-ip.example.com:16724/v1/chat/completions"},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			HTTPPath: "/health", Host: "multi-ip.example.com",
			Healthy: HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
		}},
	}}})
	p.client = &http.Client{Transport: transport}
	p.healthClients[0].Transport = transport

	firstProbe := p.probeInstance(context.Background(), 0)
	if firstProbe.err != nil || firstProbe.status != http.StatusOK {
		t.Fatalf("first resolved-address health probe = %+v, want status 200", firstProbe)
	}
	firstBody := serveChat(t, p, "")
	transport.CloseIdleConnections()
	resolvedAddress.Store(strings.TrimPrefix(second.URL, "http://"))
	secondProbe := p.probeInstance(context.Background(), 0)
	if secondProbe.err != nil || secondProbe.status != http.StatusOK {
		t.Fatalf("second resolved-address health probe = %+v, want status 200", secondProbe)
	}
	secondBody := serveChat(t, p, "")
	for name, body := range map[string]string{"first": firstBody, "second": secondBody} {
		if !strings.Contains(body, `"host":"multi-ip.example.com:16724"`) ||
			!strings.Contains(body, `"server_addr":"`+name+`"`) {
			t.Fatalf("%s resolved-address response = %q, want stable Host and selected address", name, body)
		}
	}
}

func TestHealthTargetRecomputesFromReplacementConfigWithoutDNSCache(t *testing.T) {
	check := ActiveHealthCheck{Type: "https", HTTPPath: "/health"}
	first, err := healthTarget(Instance{
		Name: "first", Override: Override{Endpoint: "https://first.example:443"},
	}, check)
	if err != nil {
		t.Fatalf("first healthTarget() error = %v", err)
	}
	replacement, err := healthTarget(Instance{
		Name: "replacement", Override: Override{Endpoint: "https://127.0.0.1:443"},
	}, check)
	if err != nil {
		t.Fatalf("replacement healthTarget() error = %v", err)
	}
	if first.Host != "first.example" || replacement.Host != "127.0.0.1" {
		t.Fatalf("health targets = (%q, %q), want config-derived hosts", first.Host, replacement.Host)
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
	if p.TaskOwner() == nil {
		t.Fatal("health task owner not initialized before PostInit")
	}
}

func TestStopHealthConcurrentWithWakes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p, tasks := newTestPluginWithTasks(t, healthProbeConfig(server.URL))

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(p.wakeHealthRefresh)
	}
	stopTestRegistry(t, tasks)
	p.Stop()
	wg.Wait()
	if active := tasks.Active(); len(active) != 0 {
		t.Fatalf("active task owners after Stop = %v", active)
	}
}

func TestStopHealthImmediatelyAfterPostInit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p, tasks := newTestPluginWithTasks(t, healthProbeConfig(server.URL))
	stopTestRegistry(t, tasks)
	p.Stop()
	p.Stop()
	if active := tasks.Active(); len(active) != 0 {
		t.Fatalf("active task owners after immediate Stop = %v", active)
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

	p, tasks := newTestPluginWithTasks(t, healthProbeConfig(server.URL))
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
	stopTestRegistry(t, tasks)
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

	p, tasks := newTestPluginWithTasks(t, healthProbeConfig(server.URL))
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
	stopTestRegistry(t, tasks)
	p.Stop()
}

func TestHealthStopJoinsInFlightRefresh(t *testing.T) {
	tasks, owner, _ := newAIHealthTestTasks(t)
	p, probeStarted, probeCanceled, release := newBlockingHealthPlugin(t, tasks, owner)
	p.wakeHealthRefresh()
	<-probeStarted

	// Generation teardown must join the owned refresher and probe before the
	// plugin closes its probe clients.
	stopped := make(chan struct{})
	go func() {
		stopTestRegistry(t, tasks)
		p.Stop()
		close(stopped)
	}()
	<-probeCanceled
	assertNotClosed(t, stopped)
	release()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("generation Stop did not join the refresher after the probe completed")
	}
	p.Stop()
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

type multiCancelAwareStreamBody struct {
	ctx     context.Context
	content *strings.Reader
	waiting chan struct{}
	once    sync.Once
}

func (b *multiCancelAwareStreamBody) Read(body []byte) (int, error) {
	if b.content.Len() > 0 {
		return b.content.Read(body)
	}
	b.once.Do(func() { close(b.waiting) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*multiCancelAwareStreamBody) Close() error { return nil }

type multiBlockingHandlerFlushWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *multiBlockingHandlerFlushWriter) Flush() {
	w.once.Do(func() { close(w.started) })
	<-w.release
	w.ResponseRecorder.Flush()
}

type multiStreamRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn multiStreamRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func TestHandlerStreamingCancellationPublishesOutcomeBeforeFlushJoin(t *testing.T) {
	flushInterval := 1
	p := newTestPlugin(t, Config{
		StreamingFlushIntervalMS: &flushInterval,
		Instances: []Instance{{
			Name: "streaming", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: "http://provider.test/v1/chat/completions"},
		}},
	})
	outcomeRecorded := make(chan struct{})
	p.streamOutcomeRecorded = func() { close(outcomeRecorded) }
	body := &multiCancelAwareStreamBody{
		content: strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n",
		),
		waiting: make(chan struct{}),
	}
	p.client.Transport = multiStreamRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body.ctx = r.Context()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`),
	).WithContext(ctx)
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	writer := &multiBlockingHandlerFlushWriter{
		ResponseRecorder: httptest.NewRecorder(),
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	handlerDone := make(chan struct{})
	go func() {
		p.Handler(http.NotFoundHandler()).ServeHTTP(writer, req)
		close(handlerDone)
	}()

	select {
	case <-body.waiting:
	case <-time.After(time.Second):
		t.Fatal("stream body did not block awaiting request cancellation")
	}
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("periodic response flush did not start")
	}
	cancel()
	select {
	case <-outcomeRecorded:
	case <-time.After(time.Second):
		t.Fatal("streaming outcome was not published before flush join")
	}
	if got := apisixctx.GetRequestVar(req, "$ai_stream_outcome"); got != string(ai_stream.StreamOutcomeCanceled) {
		t.Fatalf("$ai_stream_outcome before flush join = %#v, want canceled", got)
	}
	select {
	case <-handlerDone:
		t.Fatal("streaming handler returned while periodic Flush was blocked")
	case <-time.After(25 * time.Millisecond):
	}
	close(writer.release)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("streaming handler did not return after periodic Flush completed")
	}
}
