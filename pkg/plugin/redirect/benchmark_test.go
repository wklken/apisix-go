package redirect

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkStaticConfigPath measures the per-request redirect decision path
// with a valid static configuration. The regex_uri pattern must be compiled
// once at PostInit, never per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	p := &Plugin{config: Config{
		RegexUri: []string{"^/old/(.*)$", "/new/$1"},
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	if p.config.regexURI == nil {
		b.Fatal("regex_uri was not compiled at PostInit")
	}

	req := httptest.NewRequest(http.MethodGet, "/old/thing", nil)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		p.Handler(next).ServeHTTP(rr, req)
		if rr.Code != http.StatusFound {
			b.Fatalf("response code = %d, want 302", rr.Code)
		}
	}
}
