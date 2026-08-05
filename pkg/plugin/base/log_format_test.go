package base

import "testing"

func TestTruncateLogFormatBoundsDepthWithoutMutatingInput(t *testing.T) {
	input := map[string]any{
		"a": map[string]any{
			"b": map[string]any{
				"c": map[string]any{
					"d": map[string]any{
						"e": map[string]any{"value": "$host"},
					},
				},
			},
		},
	}

	got, truncated := TruncateLogFormat(input, 5)
	if !truncated {
		t.Fatal("TruncateLogFormat() truncated = false, want true")
	}
	level := got["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)["d"].(map[string]any)
	if child := level["e"].(map[string]any); len(child) != 0 {
		t.Fatalf("depth-five child = %#v, want empty map", child)
	}
	original := input["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)["d"].(map[string]any)["e"].(map[string]any)
	if original["value"] != "$host" {
		t.Fatalf("input was mutated: %#v", original)
	}
}

func TestTruncateLogFormatPreservesShallowValues(t *testing.T) {
	input := map[string]any{"host": "$host", "nested": map[string]any{"status": "$status"}}
	got, truncated := TruncateLogFormat(input, 5)
	if truncated {
		t.Fatal("TruncateLogFormat() truncated = true, want false")
	}
	if got["host"] != "$host" || got["nested"].(map[string]any)["status"] != "$status" {
		t.Fatalf("TruncateLogFormat() = %#v", got)
	}
}
