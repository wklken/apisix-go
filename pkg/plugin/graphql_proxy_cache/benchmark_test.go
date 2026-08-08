package graphql_proxy_cache

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkStaticConfigPath measures the per-request cache key derivation
// with valid static configuration. The config fingerprint must be computed
// once at configuration time, never formatted per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	p := newBenchmarkPlugin(b, Config{
		CacheStrategy:     "memory",
		CacheZone:         "graphql-bench-zone",
		CacheTTL:          300,
		ConsumerIsolation: boolPointer(false),
	})

	req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	body := []byte(`{"query":"{ user { id } }"}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if key := p.cacheKey(req, body); key == "" {
			b.Fatal("cacheKey() returned empty key")
		}
	}
}

func newBenchmarkPlugin(b testing.TB, cfg Config) *Plugin {
	b.Helper()
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	b.Cleanup(p.Stop)
	return p
}

func boolPointer(value bool) *bool {
	return &value
}
