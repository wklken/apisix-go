package ai_request_rewrite

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
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

func TestHandlerRewritesRequestWithOpenAICompatibleProvider(t *testing.T) {
	var llmRequest map[string]any
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("LLM method = %s, want POST", r.Method)
		}
		if got := r.URL.Path; got != "/v1/chat/completions" {
			t.Fatalf("LLM path = %s, want /v1/chat/completions", got)
		}
		if got := r.URL.Query().Get("api-version"); got != "2026-01-01" {
			t.Fatalf("api-version query = %q, want 2026-01-01", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization header = %q, want Bearer test-token", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&llmRequest); err != nil {
			t.Fatalf("decode LLM request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"content\":\"redacted\"}"}}]}`))
	}))
	defer llm.Close()

	p := newTestPlugin(t, Config{
		Prompt:   "redact sensitive fields",
		Provider: "openai-compatible",
		Auth: Auth{
			Header: map[string]string{"Authorization": "Bearer test-token"},
			Query:  map[string]string{"api-version": "2026-01-01"},
		},
		Options: map[string]any{
			"model":       "gpt-4",
			"temperature": float64(0),
		},
		Override: Override{Endpoint: llm.URL + "/v1/chat/completions"},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"content":"4111 1111 1111 1111"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := string(readTestBody(t, r))
		if got != `{"content":"redacted"}` {
			t.Fatalf("rewritten body = %q, want redacted JSON", got)
		}
		if r.ContentLength != int64(len(got)) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(got))
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
	if got := llmRequest["model"]; got != "gpt-4" {
		t.Fatalf("LLM model = %v, want gpt-4", got)
	}
	if got := llmRequest["temperature"]; got != float64(0) {
		t.Fatalf("LLM temperature = %v, want 0", got)
	}
	if got := llmRequest["stream"]; got != false {
		t.Fatalf("LLM stream = %v, want false", got)
	}
	messages, ok := llmRequest["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("LLM messages = %#v, want system and user messages", llmRequest["messages"])
	}
	if got := messages[0].(map[string]any)["content"]; got != "redact sensitive fields" {
		t.Fatalf("system message content = %v, want prompt", got)
	}
	if got := messages[1].(map[string]any)["content"]; got != `{"content":"4111 1111 1111 1111"}` {
		t.Fatalf("user message content = %v, want original request body", got)
	}
}

func TestHandlerOmitsModelForAzureOpenAI(t *testing.T) {
	var llmRequest map[string]any
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&llmRequest); err != nil {
			t.Fatalf("decode LLM request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"content\":\"redacted\"}"}}]}`))
	}))
	defer llm.Close()

	p := newTestPlugin(t, Config{
		Prompt:   "redact sensitive fields",
		Provider: "azure-openai",
		Auth:     Auth{Header: map[string]string{"api-key": "test-key"}},
		Options: map[string]any{
			"model":       "gpt-4",
			"temperature": float64(0),
		},
		Override: Override{
			Endpoint: llm.URL + "/openai/deployments/gpt-4/chat/completions?api-version=2024-02-15-preview",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"content":"4111"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := string(readTestBody(t, r))
		if got != `{"content":"redacted"}` {
			t.Fatalf("rewritten body = %q, want redacted JSON", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
	if _, ok := llmRequest["model"]; ok {
		t.Fatalf("LLM request model = %v, want omitted for azure-openai", llmRequest["model"])
	}
	if got := llmRequest["temperature"]; got != float64(0) {
		t.Fatalf("LLM request temperature = %v, want 0", got)
	}
}

func TestHandlerRegistersLLMRewriteRequestVars(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"content\":\"redacted\"}"}}]}`))
	}))
	defer llm.Close()

	p := newTestPlugin(t, Config{
		Prompt:   "redact sensitive fields",
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Options: map[string]any{
			"model":       "gpt-4",
			"temperature": float64(0),
		},
		Override: Override{Endpoint: llm.URL + "/v1/chat/completions"},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"content":"4111"}`))
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}

	body, ok := apisixctx.GetRequestVar(req, "$llm_request_body").(map[string]any)
	if !ok {
		t.Fatalf("$llm_request_body = %#v, want request body object", apisixctx.GetRequestVar(req, "$llm_request_body"))
	}
	if got := body["model"]; got != "gpt-4" {
		t.Fatalf("$llm_request_body.model = %v, want gpt-4", got)
	}
	logBody, ok := apisixlog.GetField(req, "$llm_request_body").(map[string]any)
	if !ok {
		t.Fatalf("log $llm_request_body = %#v, want request body object", apisixlog.GetField(req, "$llm_request_body"))
	}
	if got := logBody["model"]; got != "gpt-4" {
		t.Fatalf("log $llm_request_body.model = %v, want gpt-4", got)
	}
	start, ok := apisixctx.GetRequestVar(req, "$llm_request_start_time").(float64)
	if !ok || start <= 0 {
		t.Fatalf(
			"$llm_request_start_time = %#v, want positive unix seconds",
			apisixctx.GetRequestVar(req, "$llm_request_start_time"),
		)
	}
	if got := apisixctx.GetRequestVar(req, "$ai_request_body_changed"); got != true {
		t.Fatalf("$ai_request_body_changed = %#v, want true", got)
	}
}

func TestHandlerRejectsMissingRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Prompt:   "rewrite",
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
		Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for missing request body")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "missing request body") {
		t.Fatalf("response body = %q, want missing request body message", rr.Body.String())
	}
}

func TestHandlerRejectsLLMNonOKStatus(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider down", http.StatusBadGateway)
	}))
	defer llm.Close()

	p := newTestPlugin(t, Config{
		Prompt:   "rewrite",
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
		Override: Override{Endpoint: llm.URL + "/v1/chat/completions"},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"content":"hello"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called after LLM non-200 response")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "LLM service returned error status: 502") {
		t.Fatalf("response body = %q, want LLM status message", rr.Body.String())
	}
}

func TestPostInitRejectsOpenAICompatibleWithoutEndpoint(t *testing.T) {
	p := &Plugin{config: Config{
		Prompt:   "rewrite",
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

func readTestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}
