package request_context

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/observability/metrics"
)

// BenchmarkVerifiedSmallPath measures the per-request request-total increment
// on the metrics endpoint data path.
func BenchmarkVerifiedSmallPath(b *testing.B) {
	if err := metrics.Init(nil); err != nil {
		b.Fatal(err)
	}

	b.Run("gauge-inc", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			metrics.RecordHTTPRequestTotal()
		}
	})
}
