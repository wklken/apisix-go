package util

import (
	"testing"
)

// BenchmarkVerifiedHotPath measures generic config conversion from a decoded
// JSON map into a typed struct.
func BenchmarkVerifiedHotPath(b *testing.B) {
	source := map[string]any{
		"count":   2,
		"label":   "hello",
		"enabled": true,
		"ratio":   0.5,
		"tags":    []any{"a", "b", "c"},
		"meta":    map[string]any{"k": "v"},
		"nested":  map[string]any{"deep": 7},
	}

	b.Run("parse-conversion", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var target parseParityTarget
			if err := Parse(source, &target); err != nil {
				b.Fatal(err)
			}
		}
	})
}
