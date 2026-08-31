package ai_rate_limiting

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BenchmarkAIRateLimit measures the local per-request quota path: admission,
// quota-header snapshots, and response-token accounting.
func BenchmarkAIRateLimit(b *testing.B) {
	p := &Plugin{config: Config{
		Limit:         int64(1 << 60),
		TimeWindow:    60,
		LimitStrategy: "total_tokens",
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`))
	})
	handler := p.Handler(upstream)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("response code = %d, want 200", rr.Code)
		}
		if got := rr.Header().Get("X-AI-RateLimit-Limit-global"); got == "" {
			b.Fatal("quota limit header is empty")
		}
	}
}
