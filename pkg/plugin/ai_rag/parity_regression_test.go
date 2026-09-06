package ai_rag

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParityRejectsNonStringEmbeddingInputBeforeProvider(t *testing.T) {
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1]}]}`))
	}))
	defer provider.Close()

	p := newTestPlugin(t, Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{
			Endpoint: provider.URL, APIKey: "synthetic",
		}},
		VectorSearchProvider: VectorSearchProvider{AzureAISearch: AzureProvider{
			Endpoint: provider.URL, APIKey: "synthetic",
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(
		`{"ai_rag":{"embeddings":{"input":7},"vector_search":{"fields":"contentVector"}}}`,
	))
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called")
	})).ServeHTTP(response, req)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want APISIX schema rejection 400", response.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0 before request schema validation", calls.Load())
	}
}
