package response_rewrite

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BenchmarkStaticConfigPath measures the per-request response rewrite path
// with valid static configuration: compiled vars expression and compiled
// filter patterns must be reused, never recompiled per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	p := &Plugin{config: Config{
		StatusCode: 200,
		Vars:       []any{[]any{"status", "==", 200}},
		Filters: []Filter{{
			Regex: "token", Replace: "redacted", Scope: "global",
		}},
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	if p.expr == nil || p.config.Filters[0].pattern == nil {
		b.Fatal("static config was not compiled at PostInit")
	}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"secret","ok":true}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		p.Handler(upstream).ServeHTTP(rr, req)
		if !strings.Contains(rr.Body.String(), "redacted") {
			b.Fatalf("body = %q, want redacted filter applied", rr.Body.String())
		}
	}
}
