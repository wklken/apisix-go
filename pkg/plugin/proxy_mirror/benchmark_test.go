package proxy_mirror

import (
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
