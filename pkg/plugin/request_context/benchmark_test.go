package request_context

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/observability/metrics"
)

// BenchmarkVerifiedSmallPath measures the per-request request-total increment
// on the metrics endpoint data path.
func BenchmarkVerifiedSmallPath(b *testing.B) {
	metrics.Init()

	b.Run("counter-inc", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			metrics.Requests.Inc()
		}
	})
}
