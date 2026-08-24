package openfunction

import (
	"net/http"
	"net/http/httptest"
	"testing"

	function_upstream "github.com/wklken/apisix-go/pkg/plugin/function_upstream"
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
	if err := p.MaterializeSecrets(); err != nil {
		b.Fatalf("MaterializeSecrets() error = %v", err)
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
