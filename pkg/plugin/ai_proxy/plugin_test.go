package ai_proxy

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	observabilitymetrics "github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
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

func TestHandlerProxiesOpenAICompatibleChatRequest(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("upstream method = %s, want POST", r.Method)
		}
		if got := r.URL.Path; got != "/v1/chat/completions" {
			t.Fatalf("upstream path = %s, want /v1/chat/completions", got)
		}
		if got := r.URL.Query().Get("api-version"); got != "2026-01-01" {
			t.Fatalf("api-version query = %q, want 2026-01-01", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization header = %q, want Bearer test-token", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("Host"); got != "" {
			t.Fatalf("forwarded Host header = %q, want empty", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Provider", "test-llm")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}],"usage":{"total_tokens":9}}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth: Auth{
			Header: map[string]string{"Authorization": "Bearer test-token"},
			Query:  map[string]string{"api-version": "2026-01-01"},
		},
		Options: map[string]any{
			"model":       "gpt-4",
			"temperature": float64(0),
		},
		Override: Override{
			Endpoint: upstream.URL + "/v1/chat/completions",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1
	}`))
	req.Header.Set("Host", "client.example.test")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want 201", rr.Code)
	}
	if got := rr.Header().Get("X-Provider"); got != "test-llm" {
		t.Fatalf("X-Provider = %q, want test-llm", got)
	}
	if got := strings.TrimSpace(
		rr.Body.String(),
	); got != `{"choices":[{"message":{"content":"pong"}}],"usage":{"total_tokens":9}}` {
		t.Fatalf("response body = %q, want provider body", got)
	}
	if got := upstreamBody["model"]; got != "gpt-4" {
		t.Fatalf("model = %v, want gpt-4", got)
	}
	if got := upstreamBody["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want configured option overwrite", got)
	}
}

func TestHandlerMergesRequestBodyOverrideWithoutForce(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
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
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1,
	  "metadata": {"client": "caller"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Options:  map[string]any{"temperature": float64(2)},
		Override: Override{
			Endpoint:                 upstream.URL + "/v1/chat/completions",
			LLMOptions:               LLMOptions{MaxTokens: 64},
			RequestBodyForceOverride: new(true),
			RequestBody: map[string]any{
				"openai-chat": map[string]any{
					"temperature": float64(0),
					"max_tokens":  float64(8),
					"metadata": map[string]any{
						"client":  "override",
						"gateway": "apisix-go",
					},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1,
	  "metadata": {"client": "caller"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if got := upstreamBody["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want forced body override to win over options", got)
	}
	if got := upstreamBody["max_tokens"]; got != float64(8) {
		t.Fatalf("max_tokens = %v, want forced body override to win over llm_options", got)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "azure-openai",
		Auth:     Auth{Header: map[string]string{"api-key": "test-key"}},
		Options: map[string]any{
			"model":       "gpt-4",
			"temperature": float64(0),
		},
		Override: Override{
			Endpoint: upstream.URL + "/openai/deployments/gpt-4/chat/completions?api-version=2024-02-15-preview",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "model": "caller-model",
	  "messages": [{"role": "user", "content": "ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if _, ok := upstreamBody["model"]; ok {
		t.Fatalf("upstream body model = %v, want omitted for azure-openai", upstreamBody["model"])
	}
	if got := upstreamBody["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want configured option", got)
	}
}

func TestHandlerRegistersNonStreamingLLMRequestVars(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
		  "model": "gpt-4-0613",
		  "usage": {"prompt_tokens": 23, "completion_tokens": 8, "total_tokens": 31}
		}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Options:  map[string]any{"model": "gpt-4"},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "messages": [{"role": "user", "content": "ping"}]
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
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
	if got := apisixlog.GetField(req, "$llm_prompt_tokens"); got != int64(23) {
		t.Fatalf("log $llm_prompt_tokens = %v, want 23", got)
	}
}

func TestHandlerDefersProviderExecutionUntilRouteTerminal(t *testing.T) {
	events := make([]string, 0, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events = append(events, "provider")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	middle := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, ok := ai_runtime.SelectedInstanceName(r); !ok || got != "ai-proxy-openai-compatible" {
			t.Fatalf("selected instance = %q, %v", got, ok)
		}
		events = append(events, "lower-priority")
		ai_runtime.TerminalHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("ordinary upstream called for AI request")
		})).ServeHTTP(w, r)
	})
	handler := ai_runtime.EnableTerminal(p.Handler(middle))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	want := []string{"lower-priority", "provider"}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestHandlerProxiesOpenAICompatibleResponsesRequest(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/responses" {
			t.Fatalf("upstream path = %s, want /v1/responses", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
		  "model": "gpt-4.1",
		  "usage": {"input_tokens": 13, "output_tokens": 6}
		}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Options:  map[string]any{"model": "gpt-4.1"},
		Override: Override{
			Endpoint:   upstream.URL,
			LLMOptions: LLMOptions{MaxTokens: 64},
			RequestBody: map[string]any{
				"openai-responses": map[string]any{"instructions": "be concise"},
				"openai-chat":      map[string]any{"temperature": float64(0)},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"ping"}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if got := upstreamBody["instructions"]; got != "be concise" {
		t.Fatalf("instructions = %v, want protocol override", got)
	}
	if _, ok := upstreamBody["temperature"]; ok {
		t.Fatalf("temperature = %v, want chat override excluded", upstreamBody["temperature"])
	}
	if got := upstreamBody["max_output_tokens"]; got != float64(64) {
		t.Fatalf("max_output_tokens = %v, want 64", got)
	}
	if _, ok := upstreamBody["max_tokens"]; ok {
		t.Fatalf("max_tokens = %v, want omitted for Responses", upstreamBody["max_tokens"])
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_responses")
	assertLLMRequestVar(t, req, "$request_llm_model", "gpt-4.1")
	assertLLMRequestVar(t, req, "$llm_model", "gpt-4.1")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(13))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(6))
}

func TestHandlerProxiesOpenAICompatibleEmbeddingsRequest(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/embeddings" {
			t.Fatalf("upstream path = %s, want /v1/embeddings", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
		  "model": "text-embedding-3-small",
		  "usage": {"prompt_tokens": 4}
		}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Options:  map[string]any{"model": "text-embedding-3-small"},
		Override: Override{
			Endpoint:   upstream.URL + "/v1/embeddings",
			LLMOptions: LLMOptions{MaxTokens: 64},
			RequestBody: map[string]any{
				"openai-embeddings": map[string]any{"encoding_format": "float"},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"input":"ping"}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if got := upstreamBody["encoding_format"]; got != "float" {
		t.Fatalf("encoding_format = %v, want protocol override", got)
	}
	if _, ok := upstreamBody["max_tokens"]; ok {
		t.Fatalf("max_tokens = %v, want omitted for Embeddings", upstreamBody["max_tokens"])
	}
	if _, ok := upstreamBody["max_completion_tokens"]; ok {
		t.Fatalf("max_completion_tokens = %v, want omitted for Embeddings", upstreamBody["max_completion_tokens"])
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_embeddings")
	assertLLMRequestVar(t, req, "$request_llm_model", "text-embedding-3-small")
	assertLLMRequestVar(t, req, "$llm_model", "text-embedding-3-small")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(4))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(0))
}

func TestHandlerConvertsVertexEmbeddingsRequestAndResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode Vertex request: %v", err)
		}
		instances := body["instances"].([]any)
		if len(body) != 1 || instances[0].(map[string]any)["content"] != "hello" {
			t.Fatalf("Vertex request body = %#v", body)
		}
		_, _ = w.Write([]byte(`{
		  "predictions":[{"embeddings":{"values":[0.1,0.2],"statistics":{"token_count":3}}}]
		}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "vertex-ai",
		Options:  map[string]any{"model": "text-embedding-005"},
		Override: Override{Endpoint: upstream.URL + "/predict"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{
	  "model":"caller-model",
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
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(3))
}

func TestHandlerConvertsAnthropicMessagesThroughOpenAIProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode OpenAI request: %v", err)
		}
		messages := body["messages"].([]any)
		if messages[0].(map[string]any)["role"] != "system" || body["max_completion_tokens"] != float64(64) {
			t.Fatalf("converted OpenAI request = %#v", body)
		}
		tool := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		if tool["name"] != "lookup_weather" {
			t.Fatalf("converted tool = %#v", tool)
		}
		_, _ = w.Write([]byte(`{
		  "id":"chat-1","model":"provider-model",
		  "choices":[{"finish_reason":"tool_calls","message":{"content":"checking","tool_calls":[{"id":"call-1","function":{"name":"lookup_weather","arguments":"{\"city\":\"SZ\"}"}}]}}],
		  "usage":{"prompt_tokens":5,"completion_tokens":2}
		}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
	  "model":"client-model",
	  "system":"be concise",
	  "max_tokens":64,
	  "messages":[{"role":"user","content":"hello"}],
	  "tools":[{"name":"lookup.weather","input_schema":{"type":"object"}}]
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for converted Anthropic request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, body = %q", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic response: %v", err)
	}
	content := response["content"].([]any)
	toolUse := content[1].(map[string]any)
	if response["type"] != "message" || response["model"] != "provider-model" ||
		response["stop_reason"] != "tool_use" || toolUse["name"] != "lookup.weather" {
		t.Fatalf("converted Anthropic response = %#v", response)
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_chat")
	assertLLMRequestVar(t, req, "$request_llm_model", "client-model")
	assertLLMRequestVar(t, req, "$llm_model", "provider-model")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(5))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(2))
	usage := apisixctx.GetRequestVar(req, "$ai_token_usage").(map[string]any)
	if usage["total_tokens"] != int64(7) {
		t.Fatalf("$ai_token_usage = %#v", usage)
	}
}

func TestVertexEmbeddingsEndpoint(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider:     "vertex-ai",
		ProviderConf: map[string]any{"project_id": "project one", "region": "us-central1"},
		Options:      map[string]any{"model": "text embedding"},
	})
	endpoint, err := p.endpoint(ai_protocols.OpenAIEmbeddings, []byte(`{"input":"hello"}`))
	if err != nil {
		t.Fatalf("endpoint() error = %v", err)
	}
	want := "https://us-central1-aiplatform.googleapis.com/v1/projects/project%20one/locations/us-central1/" +
		"publishers/google/models/text%20embedding:predict"
	if endpoint != want {
		t.Fatalf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestHandlerRoutesBuiltInOpenAIResponsesRequest(t *testing.T) {
	var endpoint string
	var upstreamBody map[string]any
	p := newTestPlugin(t, Config{
		Provider: "openai",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Override: Override{LLMOptions: LLMOptions{MaxTokens: 64}},
	})
	p.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		endpoint = r.URL.String()
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1,"output_tokens":2}}`)),
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if endpoint != "https://api.openai.com/v1/responses" {
		t.Fatalf("endpoint = %q, want OpenAI Responses endpoint", endpoint)
	}
	if got := upstreamBody["max_output_tokens"]; got != float64(64) {
		t.Fatalf("max_output_tokens = %v, want 64", got)
	}
}

func TestHandlerBuildsAndSignsBedrockConverseRequest(t *testing.T) {
	var upstreamBody map[string]any
	var authorization string
	var sessionToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/model/claude/converse" {
			t.Fatalf("upstream path = %q, want Bedrock Converse path", got)
		}
		authorization = r.Header.Get("Authorization")
		sessionToken = r.Header.Get("X-Amz-Security-Token")
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		_, _ = w.Write([]byte(`{"usage":{"inputTokens":2,"outputTokens":1,"totalTokens":3}}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider:     "bedrock",
		ProviderConf: map[string]any{"region": "us-east-1"},
		Auth: Auth{AWS: &ai_auth.AWSConfig{
			AccessKeyID: "key", SecretAccessKey: "secret", SessionToken: "session",
		}},
		Options:  map[string]any{"model": "claude"},
		Override: Override{Endpoint: upstream.URL, LLMOptions: LLMOptions{MaxTokens: 64}},
	})
	p.now = func() time.Time { return time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC) }
	req := httptest.NewRequest(http.MethodPost, "/model/claude/converse", strings.NewReader(`{
	  "model":"caller-model",
	  "messages":[{"role":"user","content":[{"text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for Bedrock proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=key/") || sessionToken != "session" {
		t.Fatalf("authorization = %q, session token = %q", authorization, sessionToken)
	}
	if _, ok := upstreamBody["model"]; ok {
		t.Fatalf("upstream model = %#v, want omitted", upstreamBody["model"])
	}
	if got := upstreamBody["inferenceConfig"].(map[string]any)["maxTokens"]; got != float64(64) {
		t.Fatalf("inferenceConfig.maxTokens = %#v, want 64", got)
	}
}

func TestHandlerForwardsBedrockConverseEventStream(t *testing.T) {
	content := testAWSEventStreamFrame(map[string]string{
		":message-type": "event", ":event-type": "contentBlockDelta",
	}, `{"delta":{"text":"hello"}}`)
	metadata := testAWSEventStreamFrame(map[string]string{
		":message-type": "event", ":event-type": "metadata",
	}, `{"usage":{"inputTokens":4,"outputTokens":2,"totalTokens":6}}`)
	streamBody := append(content, metadata...)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/claude/converse-stream" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode Bedrock stream request: %v", err)
		}
		if _, exists := body["stream"]; exists {
			t.Fatalf("Bedrock stream flag leaked into provider body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(streamBody)
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider:     "bedrock",
		ProviderConf: map[string]any{"region": "us-east-1"},
		Auth: Auth{AWS: &ai_auth.AWSConfig{
			AccessKeyID: "key", SecretAccessKey: "secret",
		}},
		Options:  map[string]any{"model": "claude"},
		Override: Override{Endpoint: upstream.URL},
	})
	req := httptest.NewRequest(http.MethodPost, "/model/claude/converse", strings.NewReader(`{
	  "messages":[{"role":"user","content":[{"text":"hello"}]}],
	  "stream":true
	}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for Bedrock stream")
	})).ServeHTTP(rr, req)

	if rr.Body.String() != string(streamBody) || !rr.Flushed {
		t.Fatalf("Bedrock EventStream was not preserved and flushed")
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_stream")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(4))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(2))
	raw := apisixctx.GetRequestVar(req, "$llm_raw_usage").(map[string]any)
	if raw["totalTokens"] != float64(6) {
		t.Fatalf("$llm_raw_usage = %#v", raw)
	}
}

func TestHandlerAppliesGCPAccessToken(t *testing.T) {
	var authorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "vertex-ai",
		Auth:     Auth{GCP: &ai_auth.GCPConfig{ServiceAccountJSON: "test"}},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	p.gcpTokens = fakeGCPTokenApplier{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for Vertex proxy")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || authorization != "Bearer gcp-token" {
		t.Fatalf("response code = %d, authorization = %q", rr.Code, authorization)
	}
}

func TestHandlerForwardsOpenAIChatSSEAndRegistersUsage(t *testing.T) {
	streamBody := "data: {\"id\":\"one\",\"model\":\"gpt-stream\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"one\",\"model\":\"gpt-stream\",\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body["stream_options"].(map[string]any)["include_usage"] != true {
			t.Fatalf("stream_options = %#v", body["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test"}},
		Options:  map[string]any{"model": "gpt-stream"},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for streaming proxy")
	})).ServeHTTP(rr, req)

	if rr.Body.String() != streamBody {
		t.Fatalf("stream body = %q, want exact SSE body", rr.Body.String())
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_stream")
	assertLLMRequestVar(t, req, "$llm_model", "gpt-stream")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(4))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(2))
	assertUsageRequestVars(t, req, float64(4), int64(6))
}

func TestHandlerConvertsOpenAIStreamBackToAnthropicSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer client-key" || r.Header.Get("X-Api-Key") != "" ||
			r.Header.Get("Anthropic-Version") != "" {
			t.Fatalf("converted headers = %#v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode converted stream request: %v", err)
		}
		if body["stream_options"].(map[string]any)["include_usage"] != true {
			t.Fatalf("converted stream request = %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"id\":\"chat-1\",\"model\":\"gpt-stream\",\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{
		  "model":"client-model","max_tokens":64,"stream":true,
		  "messages":[{"role":"user","content":"hello"}]
		}`),
	))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for converted Anthropic stream")
	})).ServeHTTP(rr, req)

	output := rr.Body.String()
	for _, expected := range []string{
		"event: message_start", `"model":"gpt-stream"`, `"type":"text_delta"`,
		`"input_tokens":4`, "event: message_stop",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("converted Anthropic stream missing %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, `"choices"`) || strings.Contains(output, "data: [DONE]") {
		t.Fatalf("OpenAI events leaked into Anthropic stream:\n%s", output)
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_stream")
	assertLLMRequestVar(t, req, "$llm_model", "gpt-stream")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(4))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(2))
	assertUsageRequestVars(t, req, float64(4), int64(6))
}

func TestHandlerEnforcesStreamDurationAndPublishesTiming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	flushInterval := 0
	p := newTestPlugin(t, Config{
		Provider:                 "openai-compatible",
		Override:                 Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		MaxStreamDurationMS:      25,
		StreamingFlushIntervalMS: &flushInterval,
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	started := time.Now()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for bounded stream")
	})).ServeHTTP(rr, req)

	if time.Since(started) > time.Second || !rr.Flushed || !strings.Contains(rr.Body.String(), "first") {
		t.Fatalf(
			"bounded stream = (duration %s, flushed %v, body %q)",
			time.Since(started),
			rr.Flushed,
			rr.Body.String(),
		)
	}
	for _, key := range []string{"$llm_request_start_time", "$llm_time_to_first_token", "$apisix_upstream_response_time"} {
		if apisixctx.GetRequestVar(req, key) == nil {
			t.Fatalf("%s is missing", key)
		}
	}
	if apisixctx.GetRequestVar(req, "$llm_request_done") != true {
		t.Fatalf("$llm_request_done = %#v", apisixctx.GetRequestVar(req, "$llm_request_done"))
	}
}

func TestHandlerPublishesConfiguredLoggingSummaryAndPayloads(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "model":"gpt-response","choices":[{"message":{"content":"answer"}}],
		  "usage":{"prompt_tokens":3,"completion_tokens":2}
		}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		Logging:  Logging{Summaries: true, Payloads: true},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-request","messages":[{"role":"user","content":"question"}]}`),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for AI proxy")
	})).ServeHTTP(rr, req)

	summary := apisixctx.GetRequestVar(req, "$llm_summary").(map[string]any)
	if summary["request_model"] != "gpt-request" || summary["model"] != "gpt-response" ||
		summary["prompt_tokens"] != int64(3) || summary["completion_tokens"] != int64(2) {
		t.Fatalf("$llm_summary = %#v", summary)
	}
	requestLog := apisixctx.GetRequestVar(req, "$llm_request").(map[string]any)
	if requestLog["stream"] != false || len(requestLog["messages"].([]any)) != 1 {
		t.Fatalf("$llm_request = %#v", requestLog)
	}
	if responseLog := apisixctx.GetRequestVar(req, "$llm_response").(map[string]any); responseLog["content"] != "answer" {
		t.Fatalf("$llm_response = %#v", responseLog)
	}
}

func TestHandlerTracksActiveLLMConnection(t *testing.T) {
	observabilitymetrics.LLMActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_ai_proxy_active"},
		[]string{
			"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
			"request_type", "request_llm_model", "llm_model",
		},
	)
	defer func() { observabilitymetrics.LLMActiveConnections = nil }()
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"model":"gpt","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Options:  map[string]any{"model": "gpt"},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	))
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
			httptest.NewRecorder(),
			req,
		)
		close(done)
	}()
	<-entered
	gauge := observabilitymetrics.LLMActiveConnections.WithLabelValues(
		"", "", "", "", "", "", "", "", "ai_chat", "gpt", "gpt",
	)
	if got := testGaugeValue(t, gauge); got != 1 {
		t.Fatalf("active connections = %v, want 1", got)
	}
	close(release)
	<-done
	if got := testGaugeValue(t, gauge); got != 0 {
		t.Fatalf("active connections = %v, want 0", got)
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

func TestApplyLLMOptionsUsesProviderProtocolField(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		protocol    ai_protocols.Protocol
		wantField   string
		absentField string
	}{
		{
			name:        "native OpenAI Chat",
			provider:    "openai",
			protocol:    ai_protocols.OpenAIChat,
			wantField:   "max_completion_tokens",
			absentField: "max_tokens",
		},
		{
			name:        "OpenAI-compatible Chat",
			provider:    "openai-compatible",
			protocol:    ai_protocols.OpenAIChat,
			wantField:   "max_tokens",
			absentField: "max_completion_tokens",
		},
		{
			name:      "native OpenAI Responses",
			provider:  "openai",
			protocol:  ai_protocols.OpenAIResponses,
			wantField: "max_output_tokens",
		},
		{
			name:      "OpenAI-compatible Responses",
			provider:  "openai-compatible",
			protocol:  ai_protocols.OpenAIResponses,
			wantField: "max_output_tokens",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Plugin{config: Config{
				Provider: tt.provider,
				Override: Override{LLMOptions: LLMOptions{MaxTokens: 64}},
			}}
			body := map[string]any{"max_tokens": float64(8)}

			p.applyLLMOptions(body, tt.protocol)

			if got := body[tt.wantField]; got != 64 {
				t.Fatalf("%s = %#v, want 64", tt.wantField, got)
			}
			if tt.absentField != "" {
				if _, ok := body[tt.absentField]; ok {
					t.Fatalf("%s is present, want omitted", tt.absentField)
				}
			}
			if tt.protocol == ai_protocols.OpenAIResponses && body["max_tokens"] != float64(8) {
				t.Fatalf("max_tokens = %#v, want client field preserved for Responses", body["max_tokens"])
			}
		})
	}
}

func TestOpenAICompatibleEndpointUsesProtocolPathOnlyForHostOverride(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			name:     "host only",
			endpoint: "https://llm.example.test",
			want:     "https://llm.example.test/v1/responses",
		},
		{
			name:     "host with trailing slash and query",
			endpoint: "https://llm.example.test/?region=west",
			want:     "https://llm.example.test/v1/responses?region=west",
		},
		{
			name:     "OpenAI v1 base path",
			endpoint: "https://llm.example.test/v1",
			want:     "https://llm.example.test/v1/responses",
		},
		{
			name:     "full custom path and query",
			endpoint: "https://llm.example.test/custom/inference?api-version=1",
			want:     "https://llm.example.test/custom/inference?api-version=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := appendProtocolEndpoint(tt.endpoint, ai_protocols.OpenAIResponses)
			if err != nil {
				t.Fatalf("appendProtocolEndpoint() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("appendProtocolEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandlerRejectsOversizedBodyBeforeProxy(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider:       "openai-compatible",
		Auth:           Auth{Header: map[string]string{"Authorization": "Bearer token"}},
		Override:       Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
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

func TestHandlerRejectsNonJSONContentType(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
		Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`hello`))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for non-JSON content type")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "only application/json is supported") {
		t.Fatalf("response body = %q, want content-type message", rr.Body.String())
	}
}

func TestPostInitRejectsOpenAICompatibleWithoutEndpoint(t *testing.T) {
	p := &Plugin{config: Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "override.endpoint is required") {
		t.Fatalf("PostInit() error = %v, want override endpoint error", err)
	}
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

func testGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
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
