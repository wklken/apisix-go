package openwhisk

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkStaticConfigPath measures per-request OpenWhisk action request
// preparation with valid static configuration. The action path and
// authorization header must be built once, never recomputed per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	result := true
	p := &Plugin{config: Config{
		APIHost:      "https://example.openwhisk.local",
		ServiceToken: "static-token",
		Namespace:    "guest",
		Action:       "hello",
		Result:       &result,
		Timeout:      60,
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		built, err := p.buildActionRequest(req)
		if err != nil {
			b.Fatalf("buildActionRequest() error = %v", err)
		}
		if built == nil {
			b.Fatal("buildActionRequest() returned nil request")
		}
	}
}
