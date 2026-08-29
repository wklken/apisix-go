package ai_request_rewrite

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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

func TestPluginOwnsDeclaredScopedSecrets(t *testing.T) {
	var instance any = &Plugin{}
	if _, ok := instance.(base.ScopedSecretMaterializer); !ok {
		t.Fatal("ai-request-rewrite declares plugin_config secrets but does not own scoped materialization")
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

func TestHandlerUsesBedrockConverseRequestAndResponse(t *testing.T) {
	var llmRequest map[string]any
	var authorization string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/model/claude/converse" {
			t.Fatalf("LLM path = %q, want Bedrock Converse path", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&llmRequest); err != nil {
			t.Fatalf("decode LLM request: %v", err)
		}
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{
		  "output":{"message":{"role":"assistant","content":[{"text":"{\"content\":\"redacted\"}"}]}},
		  "stopReason":"end_turn"
		}`))
	}))
	defer llm.Close()

	p := newTestPlugin(t, Config{
		Prompt:       "redact sensitive fields",
		Provider:     "bedrock",
		ProviderConf: map[string]any{"region": "us-east-1"},
		Auth: Auth{AWS: &ai_auth.AWSConfig{
			AccessKeyID: "key", SecretAccessKey: "secret", SessionToken: "session",
		}},
		Options:  map[string]any{"model": "claude", "max_tokens": 128},
		Override: Override{Endpoint: llm.URL},
	})
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"content":"4111"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := string(readTestBody(t, r)); got != `{"content":"redacted"}` {
			t.Fatalf("rewritten body = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
	if _, ok := llmRequest["model"]; ok {
		t.Fatalf("Bedrock body model = %#v, want omitted", llmRequest["model"])
	}
	if llmRequest["system"] == nil || llmRequest["messages"] == nil {
		t.Fatalf("Bedrock body = %#v, want native system/messages", llmRequest)
	}
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=key/") {
		t.Fatalf("Authorization = %q, want SigV4", authorization)
	}
}

func TestAnthropicUsesMessagesProtocol(t *testing.T) {
	var llmPath string
	var llmRequest map[string]any
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&llmRequest); err != nil {
			t.Fatalf("decode LLM request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"{\"content\":\"redacted\"}"}]}`))
	}))
	defer llm.Close()

	p := newTestPlugin(t, Config{
		Prompt:   "redact sensitive fields",
		Provider: "anthropic",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
		Options: map[string]any{
			"model":       "claude-sonnet",
			"temperature": float64(0),
		},
		Override: Override{Endpoint: llm.URL},
	})
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"content":"4111 1111 1111 1111"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := string(readTestBody(t, r)); got != `{"content":"redacted"}` {
			t.Fatalf("rewritten body = %q, want redacted JSON", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
	if llmPath != "/v1/messages" {
		t.Fatalf("LLM path = %q, want /v1/messages", llmPath)
	}
	if _, ok := llmRequest["messages"]; !ok {
		t.Fatalf("LLM request = %#v, want Anthropic messages body", llmRequest)
	}
	if _, ok := llmRequest["max_completion_tokens"]; ok {
		t.Fatalf("LLM request = %#v, want no OpenAI max_completion_tokens", llmRequest)
	}
	if got := llmRequest["system"]; got != "redact sensitive fields" {
		t.Fatalf("LLM system = %#v, want Anthropic system prompt", got)
	}
	if _, ok := llmRequest["max_tokens"]; !ok {
		t.Fatalf("LLM request = %#v, want Anthropic max_tokens", llmRequest)
	}
}

func TestHandlerAppliesGCPTokenForVertexRewrite(t *testing.T) {
	var authorization string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"rewritten\":true}"}}]}`))
	}))
	defer llm.Close()
	p := newTestPlugin(t, Config{
		Prompt:   "rewrite",
		Provider: "vertex-ai",
		Auth:     Auth{GCP: &ai_auth.GCPConfig{ServiceAccountJSON: "test"}},
		Override: Override{Endpoint: llm.URL + "/v1/chat/completions"},
	})
	p.gcpTokens = fakeGCPTokenApplier{}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"input":true}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := string(readTestBody(t, r)); got != `{"rewritten":true}` {
			t.Fatalf("rewritten body = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent || authorization != "Bearer gcp-token" {
		t.Fatalf("response code = %d, Authorization = %q", rr.Code, authorization)
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
	var logMessage string
	p.warn = func(message string) {
		logMessage = message
	}

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
	if logMessage != "missing request body" {
		t.Fatalf("warning log = %q, want missing request body", logMessage)
	}
}

func TestHandlerLogsAndRejectsLLMNonOKStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusBadRequest,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, http.StatusText(status), status)
			}))
			defer llm.Close()

			p := newTestPlugin(t, Config{
				Prompt:   "rewrite",
				Provider: "openai-compatible",
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
				Override: Override{Endpoint: llm.URL + "/v1/chat/completions"},
			})
			var logMessage string
			p.logError = func(message string) {
				logMessage = message
			}

			req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader("some random content"))
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler was called after LLM non-200 response")
			})).ServeHTTP(rr, req)

			wantMessage := fmt.Sprintf("LLM service returned error status: %d", status)
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("response code = %d, want 500", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), wantMessage) {
				t.Fatalf("response body = %q, want %q", rr.Body.String(), wantMessage)
			}
			if logMessage != wantMessage {
				t.Fatalf("error log = %q, want %q", logMessage, wantMessage)
			}
		})
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

func TestDefaultProviderEndpoints(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "openai", want: "https://api.openai.com/v1/chat/completions"},
		{provider: "deepseek", want: "https://api.deepseek.com/chat/completions"},
		{provider: "aimlapi", want: "https://api.aimlapi.com/chat/completions"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			p := &Plugin{config: Config{Provider: tt.provider}}
			got, err := p.endpoint(preferredProtocol(tt.provider))
			if err != nil {
				t.Fatalf("endpoint() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("endpoint() = %q, want %q", got, tt.want)
			}
		})
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
