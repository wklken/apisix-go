package ai_rag

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestHandlerRejectsMissingAIRag(t *testing.T) {
	p := newTestPlugin(t, Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{Endpoint: "http://127.0.0.1", APIKey: "k"}},
		VectorSearchProvider: VectorSearchProvider{
			AzureAISearch: AzureProvider{Endpoint: "http://127.0.0.1", APIKey: "k"},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called without ai_rag")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if got["message"] != `request body must have "ai_rag" field` {
		t.Fatalf("response message = %q, want missing ai_rag message", got["message"])
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
	if !strings.Contains(rr.Body.String(), "rate limited") {
		t.Fatalf("response body = %q, want provider body", rr.Body.String())
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
