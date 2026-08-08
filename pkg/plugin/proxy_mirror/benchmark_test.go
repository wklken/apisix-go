package proxy_mirror

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkVerifiedHotPath measures mirror URL derivation from the static
// mirror host configuration.
func BenchmarkVerifiedHotPath(b *testing.B) {
	p := &Plugin{config: Config{
		Host:           "http://mirror.example.test:8080",
		PathConcatMode: "replace",
		SampleRatio:    1,
	}}
	if err := p.PostInit(); err != nil {
		b.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://api.example.test/orders/42?page=1", nil)

	b.Run("mirror-url", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			target, err := p.mirrorURL(request)
			if err != nil {
				b.Fatal(err)
			}
			if target == "" {
				b.Fatal("empty mirror URL")
			}
		}
	})
}

// BenchmarkStaticConfigPath measures per-request mirror request preparation
// with a valid static configuration. The request path must reuse compiled
// config instead of rebuilding it per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := &Plugin{config: Config{
		Host:        upstream.URL,
		Path:        "/mirror",
		SampleRatio: 1,
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/original", nil)
	req.Header.Set("Content-Type", "application/json")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		built, err := p.buildMirrorRequest(req, nil)
		if err != nil {
			b.Fatalf("buildMirrorRequest() error = %v", err)
		}
		if built == nil {
			b.Fatal("buildMirrorRequest() returned nil request")
		}
	}
}
