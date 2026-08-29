package ai_rag

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	return newTestPluginWithScopedValues(t, cfg, nil)
}

func newTestPluginWithScopedValues(t *testing.T, cfg Config, values map[string]string) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newScopedSecretHarness(t, name, values)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	t.Cleanup(p.Stop)
	return p
}

func TestHandlerRunsAzureRAGAndAppendsSearchResultToChat(t *testing.T) {
	var embeddingBody map[string]any
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != "embedding-key" {
			t.Fatalf("embedding api-key = %q, want embedding-key", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&embeddingBody); err != nil {
			t.Fatalf("decode embedding body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer embeddings.Close()

	var searchBody map[string]any
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != "search-key" {
			t.Fatalf("search api-key = %q, want search-key", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&searchBody); err != nil {
			t.Fatalf("decode search body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[{"title":"Azure DevOps","content":"CI/CD services"}]}`))
	}))
	defer search.Close()

	p := newTestPlugin(t, Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{
			Endpoint: embeddings.URL,
			APIKey:   "embedding-key",
		}},
		VectorSearchProvider: VectorSearchProvider{AzureAISearch: AzureProvider{
			Endpoint: search.URL,
			APIKey:   "search-key",
		}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model": "gpt-4",
	  "messages": [{"role":"user","content":"Which service is good for DevOps?"}],
	  "ai_rag": {
	    "embeddings": {"input":"Which service is good for DevOps?","dimensions":1024},
	    "vector_search": {"fields":"contentVector"}
	  }
	}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBodyForTest(t, r)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		if _, ok := got["ai_rag"]; ok {
			t.Fatalf("ai_rag still present in rewritten body: %s", body)
		}
		messages, ok := got["messages"].([]any)
		if !ok || len(messages) != 2 {
			t.Fatalf("messages = %#v, want original plus RAG message", got["messages"])
		}
		appended, ok := messages[1].(map[string]any)
		if !ok || appended["role"] != "user" || !strings.Contains(appended["content"].(string), "Azure DevOps") {
			t.Fatalf("appended message = %#v, want search result user message", messages[1])
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202, body %q", rr.Code, rr.Body.String())
	}
	if got := embeddingBody["input"]; got != "Which service is good for DevOps?" {
		t.Fatalf("embedding input = %q, want request input", got)
	}
	vectorQueries, ok := searchBody["vectorQueries"].([]any)
	if !ok || len(vectorQueries) != 1 {
		t.Fatalf("vectorQueries = %#v, want one vector query", searchBody["vectorQueries"])
	}
	query := vectorQueries[0].(map[string]any)
	if query["kind"] != "vector" || query["fields"] != "contentVector" {
		t.Fatalf("vector query = %#v, want vector kind and configured fields", query)
	}
}

func TestMaterializedAPIKeysRemainPrivateAndReachProviders(t *testing.T) {
	t.Setenv("APISIX_GO_RAG_EMBEDDING_KEY", "resolved-embedding-key")
	t.Setenv("APISIX_GO_RAG_SEARCH_KEY", "resolved-search-key")
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != "resolved-embedding-key" {
			t.Fatalf("embedding api-key = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1]}]}`))
	}))
	defer embeddings.Close()
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != "resolved-search-key" {
			t.Fatalf("search api-key = %q", got)
		}
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	defer search.Close()
	p := newTestPluginWithScopedValues(t, Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{
			Endpoint: embeddings.URL, APIKey: "$ENV://APISIX_GO_RAG_EMBEDDING_KEY",
		}},
		VectorSearchProvider: VectorSearchProvider{AzureAISearch: AzureProvider{
			Endpoint: search.URL, APIKey: "$ENV://APISIX_GO_RAG_SEARCH_KEY",
		}},
	}, map[string]string{
		"$ENV://APISIX_GO_RAG_EMBEDDING_KEY": "resolved-embedding-key",
		"$ENV://APISIX_GO_RAG_SEARCH_KEY":    "resolved-search-key",
	})
	if strings.Contains(p.config.EmbeddingsProvider.AzureOpenAI.APIKey, "resolved-embedding-key") ||
		strings.Contains(p.config.VectorSearchProvider.AzureAISearch.APIKey, "resolved-search-key") {
		t.Fatalf("materialized config exposed plaintext: %#v", p.config)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "messages":[],
	  "ai_rag":{"embeddings":{"input":"hello"},"vector_search":{"fields":"contentVector"}}
	}`))
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", response.Code, response.Body.String())
	}
	p.Stop()
}

func TestHandlerAppendsSearchResultToResponsesInput(t *testing.T) {
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2]}]}`))
	}))
	defer embeddings.Close()
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[{"content":"Use App Service"}]}`))
	}))
	defer search.Close()

	p := newTestPlugin(t, Config{
		EmbeddingsProvider:   EmbeddingsProvider{AzureOpenAI: AzureProvider{Endpoint: embeddings.URL, APIKey: "k"}},
		VectorSearchProvider: VectorSearchProvider{AzureAISearch: AzureProvider{Endpoint: search.URL, APIKey: "k"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
	  "model": "gpt-4.1",
	  "input": "hello",
	  "ai_rag": {
	    "embeddings": {"input":"hello"},
	    "vector_search": {"fields":"contentVector"}
	  }
	}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.Unmarshal(readBodyForTest(t, r), &got); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		if got["input"] != "hello\n{\"value\":[{\"content\":\"Use App Service\"}]}" {
			t.Fatalf("input = %q, want appended search result", got["input"])
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestAppendSearchResultUsesNativeMessageProtocol(t *testing.T) {
	tests := []struct {
		name string
		path string
		body map[string]any
		want func(*testing.T, map[string]any)
	}{
		{
			name: "anthropic messages",
			path: "/v1/messages",
			body: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "question"}}},
			want: func(t *testing.T, body map[string]any) {
				messages := body["messages"].([]any)
				appended := messages[1].(map[string]any)
				if appended["role"] != "user" || appended["content"] != "search result" {
					t.Fatalf("appended Anthropic message = %#v", appended)
				}
			},
		},
		{
			name: "bedrock converse",
			path: "/model/claude/converse",
			body: map[string]any{"messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{"text": "question"}},
			}}},
			want: func(t *testing.T, body map[string]any) {
				messages := body["messages"].([]any)
				appended := messages[1].(map[string]any)
				content := appended["content"].([]any)[0].(map[string]any)
				if appended["role"] != "user" || content["text"] != "search result" {
					t.Fatalf("appended Bedrock message = %#v", appended)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, test.path, nil)
			appendSearchResult(req, test.body, "search result")
			test.want(t, test.body)
		})
	}
}

func TestAppendSearchResultCreatesChatMessagesForSourceCompatibleRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/echo", nil)
	body := map[string]any{}

	appendSearchResult(req, body, "search result")

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want one RAG message", body["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["role"] != "user" || message["content"] != "search result" {
		t.Fatalf("message = %#v, want RAG user message", messages[0])
	}
}

func TestAppendSearchResultLeavesDetectedPassthroughBodyUnchanged(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/echo", nil)
	body := map[string]any{"model": "custom"}

	appendSearchResult(req, body, "search result")

	if _, ok := body["messages"]; ok {
		t.Fatalf("messages = %#v, want passthrough body unchanged", body["messages"])
	}
}

func TestHandlerRejectsInvalidRAGRequestsWithSourceDiagnostics(t *testing.T) {
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()

	p := newTestPlugin(t, Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{Endpoint: provider.URL, APIKey: "k"}},
		VectorSearchProvider: VectorSearchProvider{
			AzureAISearch: AzureProvider{Endpoint: provider.URL, APIKey: "k"},
		},
	})

	tests := []struct {
		name        string
		body        string
		want        string
		contentType string
	}{
		{
			name:        "empty body",
			want:        "{\"message\":\"could not get body: request body is empty\"}\n",
			contentType: "text/plain; charset=utf-8",
		},
		{
			name: "missing ai_rag",
			body: `{"messages":[]}`,
			want: `request body must have "ai-rag" field`,
		},
		{
			name: "missing vector search fields",
			body: `{"ai_rag":{"vector_search":{"missing-fields":"something"},"embeddings":{"input":"which service is good for devops","dimensions":1024}}}`,
			want: `request body fails schema check: property "ai_rag" validation failed: property "vector_search" validation failed: property "fields" is required`,
		},
		{
			name: "missing embeddings input",
			body: `{"ai_rag":{"vector_search":{"fields":"something"},"embeddings":{"missinginput":"which service is good for devops"}}}`,
			want: `request body fails schema check: property "ai_rag" validation failed: property "embeddings" validation failed: property "input" is required`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(test.body))
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler should not be called for invalid RAG request")
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("response code = %d, want 400", rr.Code)
			}
			if got := rr.Body.String(); got != test.want {
				t.Fatalf("response body = %q, want %q", got, test.want)
			}
			if test.contentType != "" {
				if got := rr.Header().Get("Content-Type"); got != test.contentType {
					t.Fatalf("Content-Type = %q, want %q", got, test.contentType)
				}
			}
		})
	}
	if providerCalls != 0 {
		t.Fatalf("provider calls = %d, want 0", providerCalls)
	}
}

func TestHandlerPropagatesEmbeddingProviderStatus(t *testing.T) {
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`rate limited`))
	}))
	defer embeddings.Close()

	p := newTestPlugin(t, Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{Endpoint: embeddings.URL, APIKey: "k"}},
		VectorSearchProvider: VectorSearchProvider{
			AzureAISearch: AzureProvider{Endpoint: "http://127.0.0.1", APIKey: "k"},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "ai_rag": {
	    "embeddings": {"input":"hello"},
	    "vector_search": {"fields":"contentVector"}
	  }
	}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when embedding provider fails")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("response code = %d, want 429", rr.Code)
	}
	if got := rr.Body.String(); got != "rate limited" {
		t.Fatalf("response body = %q, want plain provider body", got)
	}
}

func TestHandlerPropagatesVectorSearchProviderStatusAsPlainBody(t *testing.T) {
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[123456789]}]}`))
	}))
	defer embeddings.Close()
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}))
	defer search.Close()

	p := newTestPlugin(t, Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{Endpoint: embeddings.URL, APIKey: "k"}},
		VectorSearchProvider: VectorSearchProvider{
			AzureAISearch: AzureProvider{Endpoint: search.URL, APIKey: "k"},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(`{
	  "ai_rag": {
	    "embeddings": {"input":"hello"},
	    "vector_search": {"fields":"contentVector"}
	  }
	}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when vector search provider fails")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if got := rr.Body.String(); got != "Unauthorized" {
		t.Fatalf("response body = %q, want plain provider body", got)
	}
}

func readBodyForTest(t *testing.T, r *http.Request) []byte {
	t.Helper()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func TestPostInitDefaultsTimeoutTo30000(t *testing.T) {
	p := newTestPlugin(t, Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{Endpoint: "http://e", APIKey: "k"}},
		VectorSearchProvider: VectorSearchProvider{
			AzureAISearch: AzureProvider{Endpoint: "http://s", APIKey: "k"},
		},
	})
	if got := p.config.Timeout; got != 30000 {
		t.Fatalf("config.Timeout = %d, want default 30000", got)
	}
}

func TestHandlerTimeoutBoundsBlockedEmbeddingProvider(t *testing.T) {
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		blocked.Close()
	}()

	p := newTestPlugin(t, Config{
		Timeout: 20,
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{
			Endpoint: blocked.URL,
			APIKey:   "k",
		}},
		VectorSearchProvider: VectorSearchProvider{AzureAISearch: AzureProvider{
			Endpoint: "http://127.0.0.1:1",
			APIKey:   "k",
		}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "ai_rag": {
	    "embeddings": {"input":"hello"},
	    "vector_search": {"fields":"contentVector"}
	  }
	}`))
	rr := httptest.NewRecorder()
	started := time.Now()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when embedding provider hangs")
	})).ServeHTTP(rr, req)

	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("handler took %s, want provider timeout within 200ms", elapsed)
	}
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to request embeddings") {
		t.Fatalf("response body = %q, want embedding provider error", rr.Body.String())
	}
}
