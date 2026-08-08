package function_upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkStaticConfigPath measures the per-request function upstream
// request preparation with a valid static configuration. The request path
// must reuse compiled config instead of rebuilding it per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := &Plugin{Config: Config{FunctionURI: upstream.URL}}
	p.Name = "function-upstream"
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/function", nil)
	req.Header.Set("Content-Type", "application/json")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		built, err := p.buildRequest(req)
		if err != nil {
			b.Fatalf("buildRequest() error = %v", err)
		}
		if built == nil {
			b.Fatal("buildRequest() returned nil request")
		}
	}
}
