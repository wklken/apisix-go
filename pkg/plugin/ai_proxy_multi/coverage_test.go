package ai_proxy_multi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

type failingReadCloser struct {
	readErr  error
	closeErr error
}

func (r failingReadCloser) Read([]byte) (int, error) {
	return 0, r.readErr
}

func (r failingReadCloser) Close() error {
	return r.closeErr
}

func TestReadJSONDocumentRejectsUnsafeRequestBodies(t *testing.T) {
	plugin := &Plugin{config: Config{MaxReqBodySize: 16}}
	tests := []struct {
		name      string
		request   func() *http.Request
		wantError string
	}{
		{
			name: "unsupported content type",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "text/plain")
				return req
			},
			wantError: "unsupported content-type",
		},
		{
			name: "declared body too large",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
				req.ContentLength = 17
				return req
			},
			wantError: "exceeds max_req_body_size",
		},
		{
			name: "read error",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				req.Body = failingReadCloser{readErr: errors.New("read failed")}
				return req
			},
			wantError: "could not get body",
		},
		{
			name: "close error",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				req.Body = failingReadCloser{readErr: io.EOF, closeErr: errors.New("close failed")}
				return req
			},
			wantError: "could not get body",
		},
		{
			name: "streamed body too large",
			request: func() *http.Request {
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/chat/completions",
					strings.NewReader(`{"model":"too-large"}`),
				)
				req.ContentLength = -1
				return req
			},
			wantError: "exceeds max_req_body_size",
		},
		{
			name: "empty body",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(" \n"))
			},
			wantError: "missing request body",
		},
		{
			name: "invalid JSON",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{`))
			},
			wantError: "could not parse JSON request body",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := plugin.readJSONDocument(test.request())
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("readJSONDocument() error = %v, want containing %q", err, test.wantError)
			}
		})
	}

	plugin.config.MaxReqBodySize = 1024
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[]}`),
	)
	body, document, protocol, err := plugin.readJSONDocument(req)
	if err != nil {
		t.Fatalf("readJSONDocument(valid) error = %v", err)
	}
	if string(body) == "" || document.Raw["model"] != "gpt-test" || protocol != ai_protocols.OpenAIChat {
		t.Fatalf("readJSONDocument(valid) = %s, %#v, %#v", body, document, protocol)
	}
	replayed, err := io.ReadAll(req.Body)
	if err != nil || string(replayed) != string(body) {
		t.Fatalf("replayed request body = %q, %v; want %q", replayed, err, body)
	}
}

func TestEndpointCoversProviderAndOverrideContracts(t *testing.T) {
	plugin := &Plugin{}
	document := ai_protocols.Document{Raw: map[string]any{"model": "model/a", "stream": true}}
	providers := []Instance{
		{Provider: "openai"},
		{Provider: "deepseek"},
		{Provider: "aimlapi"},
		{Provider: "openrouter"},
		{Provider: "gemini"},
		{Provider: "anthropic"},
		{Provider: "bedrock", ProviderConf: map[string]any{"region": "us-east-1"}},
	}
	for _, instance := range providers {
		t.Run(instance.Provider, func(t *testing.T) {
			endpoint, err := plugin.endpoint(instance, ai_protocols.OpenAIChat, document)
			if err != nil || !strings.HasPrefix(endpoint, "https://") {
				t.Fatalf("endpoint(%s) = %q, %v", instance.Provider, endpoint, err)
			}
		})
	}

	for _, test := range []struct {
		name     string
		instance Instance
		protocol ai_protocols.Protocol
		contains string
	}{
		{
			name:     "passthrough override",
			instance: Instance{Provider: "custom", Override: Override{Endpoint: "https://custom.example/base"}},
			protocol: ai_protocols.Passthrough,
			contains: "https://custom.example/base",
		},
		{
			name:     "OpenAI override appends protocol path",
			instance: Instance{Provider: "openai", Override: Override{Endpoint: "https://custom.example"}},
			protocol: ai_protocols.OpenAIChat,
			contains: "/chat/completions",
		},
		{
			name: "Bedrock override appends escaped model",
			instance: Instance{
				Provider: "bedrock",
				Options:  map[string]any{"model": "model/a"},
				Override: Override{Endpoint: "https://bedrock.example"},
			},
			protocol: ai_protocols.BedrockConverse,
			contains: "model%2Fa",
		},
		{
			name:     "custom provider uses exact override",
			instance: Instance{Provider: "custom", Override: Override{Endpoint: "https://custom.example/exact"}},
			protocol: ai_protocols.OpenAIChat,
			contains: "https://custom.example/exact",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint, err := plugin.endpoint(test.instance, test.protocol, document)
			if err != nil || !strings.Contains(endpoint, test.contains) {
				t.Fatalf("endpoint() = %q, %v; want containing %q", endpoint, err, test.contains)
			}
		})
	}

	if _, err := plugin.endpoint(Instance{Provider: "custom"}, ai_protocols.OpenAIChat, document); err == nil {
		t.Fatal("endpoint(custom without override) error = nil")
	}
}

func TestVertexEndpointRequiresEmbeddingModelAndEscapesPathSegments(t *testing.T) {
	instance := Instance{Provider: "vertex-ai", ProviderConf: map[string]any{
		"project_id": "project/one",
		"region":     "us central1",
	}}
	document := ai_protocols.Document{Raw: map[string]any{}}
	chat, err := vertexEndpoint(instance, ai_protocols.OpenAIChat, document)
	if err != nil || !strings.Contains(chat, "project%2Fone") || !strings.Contains(chat, "us%20central1") {
		t.Fatalf("vertex chat endpoint = %q, %v", chat, err)
	}
	if _, err := vertexEndpoint(instance, ai_protocols.OpenAIEmbeddings, document); err == nil {
		t.Fatal("vertex embeddings endpoint accepted a missing model")
	}
	instance.Options = map[string]any{"model": "text/model"}
	embeddings, err := vertexEndpoint(instance, ai_protocols.OpenAIEmbeddings, document)
	if err != nil || !strings.Contains(embeddings, "text%2Fmodel:predict") {
		t.Fatalf("vertex embeddings endpoint = %q, %v", embeddings, err)
	}
}

func TestRequestBodyOverrideAndLLMOptionRouting(t *testing.T) {
	openAIChat := ai_protocols.OpenAIChat
	if got := requestBodyOverride(nil, openAIChat); got != nil {
		t.Fatalf("requestBodyOverride(nil) = %#v", got)
	}
	protocolValues := map[string]any{
		"openai-chat":      map[string]any{"temperature": 0.2},
		"openai-responses": map[string]any{"temperature": 0.4},
	}
	if got := requestBodyOverride(protocolValues, openAIChat); got["temperature"] != 0.2 {
		t.Fatalf("protocol request body override = %#v", got)
	}
	if got := requestBodyOverride(protocolValues, ai_protocols.BedrockConverse); got != nil {
		t.Fatalf("unmatched protocol override = %#v", got)
	}
	legacy := map[string]any{"temperature": 0.6}
	if got := requestBodyOverride(legacy, openAIChat); got["temperature"] != 0.6 {
		t.Fatalf("legacy OpenAI override = %#v", got)
	}
	if got := requestBodyOverride(legacy, ai_protocols.OpenAIResponses); got != nil {
		t.Fatalf("legacy non-chat override = %#v", got)
	}

	plugin := &Plugin{}
	for _, test := range []struct {
		name     string
		provider string
		protocol ai_protocols.Protocol
		field    string
	}{
		{name: "OpenAI chat", provider: "openai", protocol: ai_protocols.OpenAIChat, field: "max_completion_tokens"},
		{name: "OpenAI responses", provider: "openai", protocol: ai_protocols.OpenAIResponses, field: "max_output_tokens"},
		{name: "Gemini chat", provider: "gemini", protocol: ai_protocols.OpenAIChat, field: "max_completion_tokens"},
		{name: "compatible chat", provider: "openai-compatible", protocol: ai_protocols.OpenAIChat, field: "max_tokens"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{"max_tokens": 1}
			plugin.applyLLMOptions(body, test.protocol, Instance{
				Provider: test.provider,
				Override: Override{LLMOptions: LLMOptions{MaxTokens: 9}},
			})
			if body[test.field] != 9 {
				t.Fatalf("LLM options body = %#v, want %s=9", body, test.field)
			}
		})
	}
	bedrockBody := map[string]any{}
	plugin.applyLLMOptions(bedrockBody, ai_protocols.BedrockConverse, Instance{
		Provider: "bedrock",
		Override: Override{LLMOptions: LLMOptions{MaxTokens: 11}},
	})
	inference, ok := bedrockBody["inferenceConfig"].(map[string]any)
	if !ok || inference["maxTokens"] != 11 {
		t.Fatalf("Bedrock LLM options body = %#v", bedrockBody)
	}
}

func TestHashVariableCoversRequestAndNetworkSources(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://example.com/orders?id=42", nil)
	req.Host = "example.com:8443"
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Tenant", "tenant-a")
	req.AddCookie(&http.Cookie{Name: "session", Value: "cookie-a"})
	req = req.WithContext(contextWithLocalAddr(req, &net.TCPAddr{IP: net.ParseIP("198.51.100.4"), Port: 9443}))
	req = apisixctx.WithApisixVars(req, map[string]string{"$custom": "apisix-value"})
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$request_only", "request-value")

	tests := map[string]string{
		"uri":            "/orders",
		"request_uri":    "/orders?id=42",
		"query_string":   "id=42",
		"host":           "example.com",
		"remote_addr":    "192.0.2.10",
		"remote_port":    "1234",
		"server_addr":    "198.51.100.4",
		"arg_id":         "42",
		"cookie_session": "cookie-a",
		"http_x_tenant":  "tenant-a",
		"custom":         "apisix-value",
		"request_only":   "request-value",
	}
	for name, want := range tests {
		if got := hashVariable(req, name); got != want {
			t.Fatalf("hashVariable(%q) = %q, want %q", name, got, want)
		}
	}
}

func contextWithLocalAddr(r *http.Request, address net.Addr) context.Context {
	return context.WithValue(r.Context(), http.LocalAddrContextKey, address)
}
