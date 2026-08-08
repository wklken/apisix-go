package prometheus

import (
	"net/http/httptest"
	"testing"
)

// BenchmarkVerifiedHotPath measures the metrics scrape endpoint: handler
// construction plus the gather/serve path.
func BenchmarkVerifiedHotPath(b *testing.B) {
	request := httptest.NewRequest("GET", MetricsURI, nil)

	b.Run("scrape-handler", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			recorder := httptest.NewRecorder()
			MetricsHandler(recorder, request)
			if recorder.Body.Len() == 0 {
				b.Fatal("empty scrape response")
			}
		}
	})
}
