package proxy_rewrite

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkStaticConfigPath measures the per-request proxy rewrite path with
// valid static configuration. Compiled regex_uri pairs must be reused, never
// recompiled per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	p := &Plugin{config: Config{
		RegexURI: []string{"^/old/(.*)$", "/new/$1"},
		Method:   "POST",
		Headers:  Headers{Set: map[string]string{"X-Rewritten": "yes"}},
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	if len(p.config.regexURIPairs) != 1 {
		b.Fatal("regex_uri pairs were not compiled at PostInit")
	}

	req := httptest.NewRequest(http.MethodGet, "/old/thing", nil)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		p.Handler(next).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("response code = %d, want 200", rr.Code)
		}
	}
}
