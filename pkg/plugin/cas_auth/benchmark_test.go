package cas_auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkStaticConfigPath measures the per-request cas-auth session check
// with valid static configuration. The session cookie name and fingerprint
// must be derived once at PostInit, never recomputed per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	p := &Plugin{config: Config{
		IDPURI:         "https://idp.example.com/cas",
		CASCallbackURI: "/cas/callback",
		LogoutURI:      "/cas/logout",
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		p.Handler(next).ServeHTTP(rr, req)
		if rr.Code != http.StatusFound {
			b.Fatalf("status = %d, want 302 redirect to CAS", rr.Code)
		}
	}
}
