package cas_auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/testutil"
)

// BenchmarkStaticConfigPath measures the per-request cas-auth session check
// with valid static configuration. The session cookie name and fingerprint
// must be derived once at PostInit, never recomputed per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	p := &Plugin{config: Config{
		IDPURI:         "https://idp.example.com/cas",
		CASCallbackURI: "/cas/callback",
		LogoutURI:      "/cas/logout",
		Cookie: CookieConfig{
			Secret: "benchmark-cookie-secret-benchmark-cookie-secret",
		},
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	secrets, scope, closeAttempt := testutil.ScopedSecretHarness(
		b,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	b.Cleanup(closeAttempt)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		b.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
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
