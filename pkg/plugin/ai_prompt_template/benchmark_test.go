package ai_prompt_template

import "testing"

// BenchmarkVerifiedSmallPath measures per-request prompt template expansion:
// every {{expression}} in the fixed message must be resolved from values.
func BenchmarkVerifiedSmallPath(b *testing.B) {
	text := "System: answer in {{complexity}}.\nUser: explain {{prompt}} with {{depth}} detail."
	values := map[string]any{
		"complexity": "brief",
		"prompt":     "quick sort",
		"depth":      2,
	}

	b.Run("template-expansion", func(b *testing.B) {
		b.ReportAllocs()
		var sink string
		for b.Loop() {
			sink = renderString(text, values)
		}
		if sink == "" {
			b.Fatal("empty render")
		}
	})
}
