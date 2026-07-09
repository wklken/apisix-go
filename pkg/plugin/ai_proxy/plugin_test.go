package ai_proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
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
	if got := strings.TrimSpace(rr.Body.String()); got != `{"choices":[{"message":{"content":"pong"}}],"usage":{"total_tokens":9}}` {
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

func boolPtr(v bool) *bool {
	return &v
}

func assertLLMRequestVar(t *testing.T, req *http.Request, key string, want any) {
	t.Helper()

	if got := apisixctx.GetRequestVar(req, key); got != want {
		t.Fatalf("%s = %#v, want %#v", key, got, want)
	}
}
