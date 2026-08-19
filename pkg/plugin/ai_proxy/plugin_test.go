package ai_proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	observabilitymetrics "github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
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

func TestHandlerDoesNotForwardOrRegenerateAcceptEncoding(t *testing.T) {
	var acceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
		Override: Override{Endpoint: upstream.URL},
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate")

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy")
	})).ServeHTTP(httptest.NewRecorder(), req)

	if acceptEncoding != "" {
		t.Fatalf("upstream Accept-Encoding = %q, want absent", acceptEncoding)
	}
}

func TestBuildProviderRequestUsesOfficialOpenAIEndpointAndAuth(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider: "openai",
		Auth:     Auth{Header: map[string]string{"Authorization": "some-key"}},
		Options: map[string]any{
			"model":       "gpt-4",
			"max_tokens":  float64(512),
			"temperature": float64(1),
		},
	})
	clientRequest := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	clientRequest.Header.Set("Content-Type", "application/json")

	body, _, protocol, err := p.readJSONDocument(clientRequest)
	if err != nil {
		t.Fatalf("readJSONDocument() error = %v", err)
	}
	providerRequest, err := p.buildProviderRequest(clientRequest, body, protocol)
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	if got := providerRequest.URL.String(); got != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("provider URL = %q, want official OpenAI chat endpoint", got)
	}
	if got := providerRequest.Header.Get("Authorization"); got != "some-key" {
		t.Fatalf("provider Authorization = %q, want configured key", got)
	}
	var providerBody map[string]any
	if err := json.NewDecoder(providerRequest.Body).Decode(&providerBody); err != nil {
		t.Fatalf("decode provider body: %v", err)
	}
	if providerBody["model"] != "gpt-4" ||
		providerBody["max_tokens"] != float64(512) ||
		providerBody["temperature"] != float64(1) {
		t.Fatalf("provider body = %#v, want configured model and options", providerBody)
	}
}

func TestBuildProviderRequestPreservesPassthroughRouting(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider: "openai",
		Auth: Auth{
			Header: map[string]string{"Authorization": "Bearer token"},
			Query:  map[string]string{"akey": "secret"},
		},
		Override: Override{
			Endpoint: "https://provider.example?name=fromendpoint&ekey=eval",
		},
	})
	clientRequest := httptest.NewRequest(
		http.MethodPut,
		"/v1/images/generations?name=fromclient&ckey=cval&akey=attacker",
		strings.NewReader(`{"prompt":"x"}`),
	)
	clientRequest.Header.Set("Content-Type", "application/json")

	body, _, protocol, err := p.readJSONDocument(clientRequest)
	if err != nil {
		t.Fatalf("readJSONDocument() error = %v", err)
	}
	if protocol != ai_protocols.Passthrough {
		t.Fatalf("protocol = %#v, want passthrough", protocol)
	}
	providerRequest, err := p.buildProviderRequest(clientRequest, body, protocol)
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	if providerRequest.Method != http.MethodPut {
		t.Fatalf("provider method = %q, want PUT", providerRequest.Method)
	}
	wantURL := "https://provider.example/v1/images/generations?" +
		"akey=secret&ckey=cval&ekey=eval&name=fromclient"
	if got := providerRequest.URL.String(); got != wantURL {
		t.Fatalf("provider URL = %q, want %q", got, wantURL)
	}
}

func TestReadJSONBodyRejectsNullWithoutPanicking(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider: "openai",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("null"))
	req.Header.Set("Content-Type", "application/json")

	if _, _, _, err := p.readJSONDocument(req); err == nil {
		t.Fatal("readJSONDocument() error = nil, want unsupported null body")
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

func TestSchemaRejectsUnknownRequestBodyProtocolKey(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"provider": "openai",
		"auth":     map[string]any{"header": map[string]any{"Authorization": "Bearer t"}},
		"override": map[string]any{
			"request_body": map[string]any{"not-a-protocol": map[string]any{"x": 1}},
		},
	}

	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("unknown request_body protocol key was accepted")
	}
}

func TestHandlerPreservesRawBodyWhenProviderRequestIsUnchanged(t *testing.T) {
	const raw = `{ "messages" : [ { "role" : "user", "content" : "hi" } ], "temperature" : 0.7 }`
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
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if upstreamBody != raw {
		t.Fatalf("upstream body = %q, want exact raw body %q", upstreamBody, raw)
	}
}

func TestHandlerDeepMergesRequestBodyIntoGeneratedStreamOptions(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
		Override: Override{
			Endpoint: upstream.URL + "/v1/chat/completions",
			RequestBody: map[string]any{
				"openai-chat": map[string]any{
					"stream_options": map[string]any{"extra": float64(1)},
				},
			},
		},
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/chat",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)

	streamOptions, ok := upstreamBody["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v, want object", upstreamBody["stream_options"])
	}
	if streamOptions["include_usage"] != true || streamOptions["extra"] != float64(1) {
		t.Fatalf("stream_options = %#v, want generated include_usage and configured extra", streamOptions)
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
	const upstreamResponse = `{
		  "model": "gpt-4-0613",
		  "usage": {"prompt_tokens": 23, "completion_tokens": 8, "total_tokens": 31}
		}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamResponse))
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

func TestHandlerLeavesUpstreamResponseVarsUnsetWhenProviderRequestFails(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstreamAddress := strings.TrimPrefix(upstream.URL, "http://")
	upstreamHost, _, _ := strings.Cut(upstreamAddress, ":")
	upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Options:  map[string]any{"model": "gpt-4"},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"messages":[{"role":"user","content":"ping"}]}`),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
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

func TestHandlerRejectsInvalidAnthropicMessagesBeforeUpstream(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing messages", body: `{"model":"claude-3-5-sonnet-20241022"}`},
		{name: "non-array messages", body: `{"model":"claude-3-5-sonnet-20241022","messages":"hello"}`},
		{name: "empty messages", body: `{"model":"claude-3-5-sonnet-20241022","messages":[]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls int
			p := newTestPlugin(t, Config{
				Provider: "openai-compatible",
				Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
			})
			p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalls++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next handler called for invalid Anthropic request")
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("response code = %d, want 400; body = %q", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "missing messages") {
				t.Fatalf("response body = %q, want missing messages", rr.Body.String())
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
			}
		})
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

func TestHandlerRejectsInvalidBedrockRequestBeforeUpstream(t *testing.T) {
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
			path:       "/single-ai/body-model-only/converse",
			body:       `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "could not resolve upstream path",
		},
		{
			name:       "non Converse protocol",
			options:    map[string]any{"model": "claude"},
			path:       "/single-ai/bedrock-chat",
			body:       `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "does not support openai-chat protocol",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls int
			p := newTestPlugin(t, Config{
				Provider:     "bedrock",
				ProviderConf: map[string]any{"region": "us-east-1"},
				Auth: Auth{AWS: &ai_auth.AWSConfig{
					AccessKeyID: "key", SecretAccessKey: "secret",
				}},
				Options:  test.options,
				Override: Override{Endpoint: "http://127.0.0.1:1"},
			})
			p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalls++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			})
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
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
			}
		})
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
	if got := apisixctx.GetRequestVar(req, "$ai_stream_outcome"); got != string(ai_stream.StreamOutcomeSuccess) {
		t.Fatalf("$ai_stream_outcome = %#v, want success", got)
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
	streamBody := "data: {\"id\":\"chat-1\",\"model\":\"gpt-stream\",\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
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
		w.Header().Set("Content-Length", strconv.Itoa(len(streamBody)))
		_, _ = w.Write([]byte(streamBody))
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
	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("converted Anthropic stream Content-Length = %q, want omitted", got)
	}
	assertLLMRequestVar(t, req, "$request_type", "ai_stream")
	assertLLMRequestVar(t, req, "$llm_model", "gpt-stream")
	assertLLMRequestVar(t, req, "$llm_prompt_tokens", int64(4))
	assertLLMRequestVar(t, req, "$llm_completion_tokens", int64(2))
	assertUsageRequestVars(t, req, float64(4), int64(6))
}

func TestHandlerPreservesUpstreamStatusForEmptySSE(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "no content", status: http.StatusNoContent},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(test.status)
			}))
			defer upstream.Close()

			p := newTestPlugin(t, Config{
				Provider: "openai-compatible",
				Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
			  "model":"test-model","stream":true,
			  "messages":[{"role":"user","content":"Hi"}]
			}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next handler called for empty stream")
			})).ServeHTTP(rr, req)

			if rr.Code != test.status {
				t.Fatalf("response code = %d, want %d; body = %q", rr.Code, test.status, rr.Body.String())
			}
			if rr.Body.Len() != 0 {
				t.Fatalf("response body = %q, want empty", rr.Body.String())
			}
		})
	}
}

func TestHandlerReturnsBadGatewayWhenAnthropicStreamProducesNoOutput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"type\":\"message\"}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
				"event: message_stop\n" +
				"data: {}\n\n",
		))
	}))
	defer upstream.Close()

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("ai-proxy-stream-no-output-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "streaming response completed without producing any output") {
			entries <- entry
		}
	})
	defer stop()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/messages"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
	  "model":"test-model","stream":true,
	  "messages":[{"role":"user","content":"Hi"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for mismatched stream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want 502; body = %q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "streaming response completed without producing any output") {
		t.Fatalf("response body = %q, want no-output message", rr.Body.String())
	}
	select {
	case entry := <-entries:
		if entry.Level != "ERROR" {
			t.Fatalf("log level = %s, want ERROR", entry.Level)
		}
	case <-time.After(time.Second):
		t.Fatal("mismatched stream error was not logged")
	}
}

func TestHandlerWarnsWhenStreamingClientContextCancels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"tok\"},\"finish_reason\":null}]}\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("ai-proxy-stream-cancel-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "client disconnected during AI streaming") {
			entries <- entry
		}
	})
	defer stop()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"test-model","stream":true,
	  "messages":[{"role":"user","content":"Hi"}]
	}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler := p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for streaming request")
	}))
	go handler.ServeHTTP(rr, req)
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case entry := <-entries:
		if entry.Level != "WARN" {
			t.Fatalf("log level = %s, want WARN", entry.Level)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect was not logged at warning level")
	}
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

func TestHandlerLogsStreamDurationExceeded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"tok\"},\"finish_reason\":null}]}\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("ai-proxy-stream-duration-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "max_stream_duration_ms exceeded") {
			entries <- entry
		}
	})
	defer stop()

	flushInterval := 0
	p := newTestPlugin(t, Config{
		Provider:                 "openai-compatible",
		Override:                 Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		MaxStreamDurationMS:      50,
		StreamingFlushIntervalMS: &flushInterval,
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"test-model","stream":true,
	  "messages":[{"role":"user","content":"Hi"}]
	}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for bounded stream")
	})).ServeHTTP(rr, req)

	select {
	case entry := <-entries:
		if entry.Level != "ERROR" {
			t.Fatalf("log level = %s, want ERROR", entry.Level)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream duration abort was not logged")
	}
	if got := apisixctx.GetRequestVar(req, "$ai_stream_outcome"); got != string(ai_stream.StreamOutcomeCanceled) {
		t.Fatalf("$ai_stream_outcome = %#v, want canceled", got)
	}
}

type blockingRequestBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingRequestBody() *blockingRequestBody {
	return &blockingRequestBody{closed: make(chan struct{})}
}

func (b *blockingRequestBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, os.ErrDeadlineExceeded
}

func (b *blockingRequestBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestTransportTimesOutStalledRequestBodySend(t *testing.T) {
	p := newTestPlugin(t, Config{Timeout: 30})
	transport := p.transport()
	request, err := http.NewRequest(http.MethodPost, "http://example.com", newBlockingRequestBody())
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

func TestHandlerRejectsNonStreamingOversizedContentLength(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write([]byte(strings.Repeat("x", 100000)))
	}))
	defer upstream.Close()

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("ai-proxy-cl-precheck-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "exceeds max_response_bytes") {
			entries <- entry
		}
	})
	defer stop()

	p := newTestPlugin(t, Config{
		Provider:         "openai-compatible",
		Override:         Override{Endpoint: upstream.URL + "/v1/oversized"},
		MaxResponseBytes: 1024,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"test-model",
	  "messages":[{"role":"user","content":"Hi"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for oversized response")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want 502; body = %q", rr.Code, rr.Body.String())
	}
	select {
	case entry := <-entries:
		if !strings.Contains(entry.Message, "Content-Length 100000 exceeds max_response_bytes 1024") {
			t.Fatalf("log message = %q", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("oversized Content-Length was not logged")
	}
}

func TestHandlerRejectsNonStreamingOversizedChunkedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, _ := w.(http.Flusher)
		for range 10 {
			_, _ = w.Write([]byte(strings.Repeat("x", 10000)))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("ai-proxy-chunked-limit-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "exceeds max_response_bytes") {
			entries <- entry
		}
	})
	defer stop()

	p := newTestPlugin(t, Config{
		Provider:         "openai-compatible",
		Override:         Override{Endpoint: upstream.URL + "/v1/oversized_chunked"},
		MaxResponseBytes: 1024,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"test-model",
	  "messages":[{"role":"user","content":"Hi"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for oversized chunked response")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want 502; body = %q", rr.Code, rr.Body.String())
	}
	select {
	case entry := <-entries:
		if !strings.Contains(entry.Message, "exceeds max_response_bytes 1024") {
			t.Fatalf("log message = %q", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("oversized chunked body was not logged")
	}
}

func TestRegisterLLMTokenDetailVarsSupportsResponsesDetails(t *testing.T) {
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	registerLLMTokenDetailVars(req, map[string]any{
		"input_tokens":  20,
		"output_tokens": 5,
		"input_tokens_details": map[string]any{
			"cached_tokens": 10,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 3,
		},
	})
	assertLLMRequestVar(t, req, "$llm_cache_read_input_tokens", int64(10))
	assertLLMRequestVar(t, req, "$llm_reasoning_tokens", int64(3))
}

func TestRegisterLLMMetadataVarsCountsTopLevelResponsesToolCalls(t *testing.T) {
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	registerLLMMetadataVars(req, []byte(`{"input":"What is the weather?","model":"gpt-4o-mini"}`), []byte(`{
	  "id":"r1","object":"response","model":"gpt-4o-mini",
	  "output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{}"}],
	  "usage":{"input_tokens":40,"output_tokens":20,"total_tokens":60}
	}`), nil)
	assertLLMRequestVar(t, req, "$llm_has_tool_calls", true)
	assertLLMRequestVar(t, req, "$llm_tool_count", 1)
}

func TestHandlerRegistersLLMMetadataVarsForToolCallsAndCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
		  "model":"gpt-4o","choices":[{"message":{"content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"t2","type":"function","function":{"name":"g","arguments":"{}"}}]}}],
		  "usage":{"prompt_tokens":30,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10,"cache_creation_input_tokens":5},"completion_tokens_details":{"reasoning_tokens":7}}
		}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"user":"alice"
	}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called")
	})).ServeHTTP(rr, req)

	assertLLMRequestVar(t, req, "$llm_end_user_id", "alice")
	assertLLMRequestVar(t, req, "$llm_has_tool_calls", true)
	assertLLMRequestVar(t, req, "$llm_tool_count", 2)
	assertLLMRequestVar(t, req, "$llm_cache_read_input_tokens", int64(10))
	assertLLMRequestVar(t, req, "$llm_cache_creation_input_tokens", int64(5))
	assertLLMRequestVar(t, req, "$llm_reasoning_tokens", int64(7))
}

func TestHandlerRegistersLLMMetadataVarsForSafetyIdentifier(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(
			[]byte(
				`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			),
		)
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"safety_identifier":"user-xyz"
	}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called")
	})).ServeHTTP(rr, req)

	assertLLMRequestVar(t, req, "$llm_end_user_id", "user-xyz")
	assertLLMRequestVar(t, req, "$llm_has_tool_calls", false)
	assertLLMRequestVar(t, req, "$llm_tool_count", 0)
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
			got, err := ai_protocols.AppendProtocolEndpoint(tt.endpoint, ai_protocols.OpenAIResponses)
			if err != nil {
				t.Fatalf("ai_protocols.AppendProtocolEndpoint() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ai_protocols.AppendProtocolEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenAIOverrideHostUsesProtocolPath(t *testing.T) {
	p := &Plugin{config: Config{
		Provider: "openai",
		Override: Override{Endpoint: "https://llm.example.test"},
	}}

	got, err := p.endpoint(ai_protocols.OpenAIResponses, []byte(`{"model":"gpt-4o"}`))
	if err != nil {
		t.Fatalf("endpoint() error = %v", err)
	}
	if got != "https://llm.example.test/v1/responses" {
		t.Fatalf("endpoint() = %q, want OpenAI Responses protocol path", got)
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

func TestHandlerProgressingStreamSurvivesConfiguredTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := range 12 {
			chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"tok" + strconv.Itoa(i) + "\"}}]}\n\n"
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Timeout:  500,
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
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
	for i := range 12 {
		if !strings.Contains(output, "tok"+strconv.Itoa(i)) {
			t.Fatalf("progressing stream lost chunk %d; body = %q", i, output)
		}
	}
}

func TestHandlerStalledStreamTimesOutConfiguredInactivity(t *testing.T) {
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
		Provider: "openai-compatible",
		Timeout:  100,
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
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
