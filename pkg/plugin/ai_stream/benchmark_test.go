package ai_stream

import (
	"strings"
	"testing"
)

// BenchmarkStreamUsageAccumulation measures per-chunk usage text accumulation
// (Usage.AppendText) at 100 chunks of 100B (10KiB total payload).
func BenchmarkStreamUsageAccumulation(b *testing.B) {
	chunk := strings.Repeat("a", 100)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var usage Usage
		for range 100 {
			usage.AppendText(chunk)
		}
		if len(usage.Text) == 0 {
			b.Fatal("accumulation produced no text")
		}
	}
}
