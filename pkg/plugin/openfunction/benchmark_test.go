package openfunction

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	function_upstream "github.com/wklken/apisix-go/pkg/plugin/function_upstream"
	"github.com/wklken/apisix-go/pkg/testutil"
)

// BenchmarkStaticConfigPath measures the per-request openfunction request
// processing with valid static configuration. The service token must be
// resolved once, never rebuilt per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	p := &Plugin{config: Config{Authorization: &Authorization{
		ServiceToken: "static-service-token",
	}}}
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

	req := httptest.NewRequest(http.MethodPost, "/", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.processRequest(req, function_upstream.Config{})
		if got := req.Header.Get("Authorization"); got != "Basic c3RhdGljLXNlcnZpY2UtdG9rZW4=" {
			b.Fatalf("Authorization = %q, want static service token", got)
		}
	}
}
