package ai_proxy_multi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParitySingleInstance5xxDoesNotExposeProviderBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Provider", "must-not-leak")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"provider-private-detail"}}`))
	}))
	t.Cleanup(upstream.Close)

	maxRetries := 0
	p := newTestPlugin(t, Config{
		FallbackStrategy: "http_5xx",
		MaxRetries:       &maxRetries,
		Instances: []Instance{{
			Name: "only", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		}},
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("provider error body exposed = %q, want empty APISIX 3.17 response", response.Body.String())
	}
	if response.Header().Get("Content-Type") != "" || response.Header().Get("X-Provider") != "" {
		t.Fatalf("provider error headers exposed = %#v, want none", response.Header())
	}
}
