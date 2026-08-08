package data_mask

import (
	"testing"
)

// BenchmarkVerifiedHotPath measures recursive JSON path masking across a
// multi-level document.
func BenchmarkVerifiedHotPath(b *testing.B) {
	root := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": map[string]any{
					"secret": "value",
				},
			},
		},
		"items": []any{
			map[string]any{"secret": "a"},
			map[string]any{"secret": "b"},
			map[string]any{"nested": map[string]any{"secret": "c"}},
		},
	}
	rule := MaskRule{
		Name:   "$..secret",
		Action: "replace",
		Value:  "***",
	}

	b.Run("multi-segment-mask", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			masked := maskJSONPath(root, rule)
			if !masked {
				b.Fatal("recursive mask did not match")
			}
		}
	})
}
