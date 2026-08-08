package ai_protocols

import (
	"strings"
	"testing"
)

func TestAnthropicMediaConversionHandlesURLBase64AndDefaults(t *testing.T) {
	urlImage := convertAnthropicMedia(map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": "https://example.com/image.png"},
	})
	if image, ok := urlImage["image_url"].(map[string]any); !ok || image["url"] != "https://example.com/image.png" {
		t.Fatalf("URL image conversion = %#v", urlImage)
	}

	for _, test := range []struct {
		name     string
		block    map[string]any
		contains string
	}{
		{
			name: "image default media type",
			block: map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "base64", "data": "aW1hZ2U="},
			},
			contains: "data:image/png;base64,aW1hZ2U=",
		},
		{
			name: "document default media type",
			block: map[string]any{
				"type":   "document",
				"source": map[string]any{"type": "base64", "data": "cGRm"},
			},
			contains: "data:application/pdf;base64,cGRm",
		},
		{
			name: "explicit media type",
			block: map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "data": "aW1hZ2U=", "media_type": "image/jpeg",
				},
			},
			contains: "data:image/jpeg;base64,aW1hZ2U=",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			converted := convertAnthropicMedia(test.block)
			image, ok := converted["image_url"].(map[string]any)
			if !ok || image["url"] != test.contains {
				t.Fatalf("convertAnthropicMedia() = %#v", converted)
			}
		})
	}

	for _, block := range []map[string]any{
		{"type": "image", "source": map[string]any{"type": "url", "url": ""}},
		{"type": "image", "source": map[string]any{"type": "file", "data": "x"}},
		{"type": "image", "source": map[string]any{"type": "base64", "data": ""}},
	} {
		if got := convertAnthropicMedia(block); got != nil {
			t.Fatalf("invalid media conversion = %#v", got)
		}
	}
}

func TestAnthropicToolConversionSanitizesDeduplicatesAndSkipsBuiltins(t *testing.T) {
	longName := strings.Repeat("tool", 30)
	tools, names := convertAnthropicTools([]any{
		map[string]any{"type": "web_search_20250305", "name": "builtin"},
		map[string]any{"name": "bad tool/name", "description": "first", "input_schema": map[string]any{}},
		map[string]any{"name": "bad tool/name", "description": "second", "input_schema": map[string]any{}},
		map[string]any{"name": longName, "input_schema": map[string]any{}},
		map[string]any{"description": "missing name"},
	})
	if len(tools) != 3 {
		t.Fatalf("converted tools = %#v, want three non-builtins", tools)
	}
	first := tools[0].(map[string]any)["function"].(map[string]any)["name"]
	second := tools[1].(map[string]any)["function"].(map[string]any)["name"]
	third := tools[2].(map[string]any)["function"].(map[string]any)["name"].(string)
	if first != "bad_tool_name" || second != "bad_tool_name_2" || len(third) > openAIToolNameMaxLength {
		t.Fatalf("sanitized names = %q, %q, %q", first, second, third)
	}
	if names[first.(string)] != "bad tool/name" || names[second.(string)] != "bad tool/name" {
		t.Fatalf("tool name map = %#v", names)
	}
}

func TestAnthropicToolResultPreservesMediaAndFlattensText(t *testing.T) {
	if got := convertAnthropicToolResult("plain"); got != "plain" {
		t.Fatalf("string tool result = %#v", got)
	}
	textOnly := convertAnthropicToolResult([]any{
		map[string]any{"type": "text", "text": "one"},
		map[string]any{"type": "text", "text": "two"},
	})
	if textOnly != "onetwo" {
		t.Fatalf("text tool result = %#v", textOnly)
	}
	withMedia := convertAnthropicToolResult([]any{
		map[string]any{"type": "text", "text": "caption"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "aW1hZ2U="}},
	})
	if parts, ok := withMedia.([]any); !ok || len(parts) != 2 {
		t.Fatalf("media tool result = %#v", withMedia)
	}
}

func TestAnthropicThinkingAndToolChoiceMappings(t *testing.T) {
	for _, test := range []struct {
		budget float64
		want   string
	}{
		{budget: 1000, want: "minimal"},
		{budget: 1500, want: "low"},
		{budget: 3000, want: "medium"},
		{budget: 5000, want: "high"},
	} {
		dst := map[string]any{}
		convertAnthropicThinking(dst, map[string]any{"type": "enabled", "budget_tokens": test.budget}, nil)
		if dst["reasoning_effort"] != test.want {
			t.Fatalf("budget %v reasoning effort = %#v", test.budget, dst)
		}
	}
	for _, outputConfig := range []any{nil, map[string]any{"effort": "high"}} {
		dst := map[string]any{}
		convertAnthropicThinking(dst, map[string]any{"type": "adaptive"}, outputConfig)
		if dst["reasoning_effort"] == "" {
			t.Fatalf("adaptive reasoning effort = %#v", dst)
		}
	}

	for kind, want := range map[string]any{"auto": "auto", "any": "required", "none": "none"} {
		dst := map[string]any{}
		convertAnthropicToolChoice(dst, map[string]any{"type": kind, "disable_parallel_tool_use": true})
		if dst["tool_choice"] != want || dst["parallel_tool_calls"] != false {
			t.Fatalf("tool choice %q = %#v", kind, dst)
		}
	}
	dst := map[string]any{}
	convertAnthropicToolChoice(dst, map[string]any{"type": "tool", "name": "lookup"})
	choice := dst["tool_choice"].(map[string]any)
	if choice["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("named tool choice = %#v", dst)
	}
}

func TestAnthropicErrorStopReasonAndUsageMappings(t *testing.T) {
	if got := convertOpenAIError("failure"); got["type"] != "api_error" || got["message"] != "failure" {
		t.Fatalf("string error = %#v", got)
	}
	if got := convertOpenAIError(
		map[string]any{"code": "rate_limit", "message": "slow down"},
	); got["type"] != "rate_limit" ||
		got["message"] != "slow down" {
		t.Fatalf("object error = %#v", got)
	}
	for input, want := range map[string]string{
		"length":        "max_tokens",
		"tool_calls":    "tool_use",
		"function_call": "tool_use",
		"stop":          "end_turn",
	} {
		if got := anthropicStopReason(input); got != want {
			t.Fatalf("anthropicStopReason(%q) = %q, want %q", input, got, want)
		}
	}
	usage := anthropicUsage(map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": int64(4),
		"prompt_tokens_details": map[string]any{
			"cached_tokens":               3,
			"cache_creation_input_tokens": 2,
		},
	})
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(4) ||
		usage["cache_read_input_tokens"] != float64(3) {
		t.Fatalf("anthropic usage = %#v", usage)
	}
	for value, want := range map[any]float64{float64(1.5): 1.5, int64(2): 2, int(3): 3, "4": 0} {
		if got := numericFloat(value); got != want {
			t.Fatalf("numericFloat(%#v) = %v, want %v", value, got, want)
		}
	}
}

func TestProtocolStreamingAndNumericUsageEdgeTypes(t *testing.T) {
	document := Document{Raw: map[string]any{"stream": true}}
	if !document.IsStreaming(OpenAIChat) || !IsStreaming(OpenAIChat, document.Raw) {
		t.Fatal("OpenAI streaming flag was not detected")
	}
	if (Document{}).IsStreaming(OpenAIEmbeddings) || IsStreaming(OpenAIEmbeddings, document.Raw) {
		t.Fatal("embeddings incorrectly reported streaming")
	}
	for value, want := range map[any]int64{float64(1.6): 2, int64(3): 3, int(4): 4, "5": -1} {
		if got := NumericUsage(value, true); got != want {
			t.Fatalf("numericUsage(%#v) = %d, want %d", value, got, want)
		}
	}
}
