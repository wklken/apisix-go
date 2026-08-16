package metrics

import "testing"

// BenchmarkVerifiedSmallPath measures the request-total increment that the
// request-context plugin performs for every request.
func BenchmarkVerifiedSmallPath(b *testing.B) {
	if err := Init(); err != nil {
		b.Fatal(err)
	}

	b.Run("counter-inc", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			Requests.Inc()
		}
	})
}
