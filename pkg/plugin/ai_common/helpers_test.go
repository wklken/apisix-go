package ai_common

import (
	"net/http"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

func TestCloneJSONValueDoesNotAlias(t *testing.T) {
	source := map[string]any{
		"a": "1",
		"nested": map[string]any{
			"list": []any{1, 2, map[string]any{"x": true}},
		},
	}
	clone := CloneJSONValue(source).(map[string]any)

	nestedClone := clone["nested"].(map[string]any)
	nestedClone["list"].([]any)[2].(map[string]any)["x"] = false

	if source["nested"].(map[string]any)["list"].([]any)[2].(map[string]any)["x"] != true {
		t.Fatal("mutating the clone changed the source")
	}
}

func TestAsAnyMap(t *testing.T) {
	if _, ok := AsAnyMap(map[string]any{"a": 1}); !ok {
		t.Fatal("AsAnyMap(map) = false, want true")
	}
	if _, ok := AsAnyMap([]any{1}); ok {
		t.Fatal("AsAnyMap(slice) = true, want false")
	}
}

func TestMergeBodyMap(t *testing.T) {
	dst := map[string]any{
		"a": "old",
		"nested": map[string]any{
			"x": 1,
			"y": 2,
		},
	}

	MergeBodyMap(dst, map[string]any{
		"a": "new",
		"b": "added",
		"nested": map[string]any{
			"y": 20,
		},
	}, false)

	if got := dst["a"]; got != "old" {
		t.Fatalf("a = %v, want old (force=false keeps existing)", got)
	}
	if got := dst["b"]; got != "added" {
		t.Fatalf("b = %v, want added", got)
	}
	if got := dst["nested"].(map[string]any)["y"]; got != 2 {
		t.Fatalf("nested.y = %v, want 2 (non-force merge keeps existing)", got)
	}

	MergeBodyMap(dst, map[string]any{"a": "forced"}, true)
	if got := dst["a"]; got != "forced" {
		t.Fatalf("a = %v, want forced", got)
	}
}

func TestCopyForwardHeadersSkipsHopByHop(t *testing.T) {
	src := http.Header{
		"Host":            {"example.com"},
		"Content-Length":  {"10"},
		"Accept-Encoding": {"gzip"},
		"X-Custom":        {"v1", "v2"},
	}
	dst := http.Header{}

	CopyForwardHeaders(dst, src)

	if len(dst) != 1 {
		t.Fatalf("copied headers = %#v, want only X-Custom", dst)
	}
	if values := dst.Values("X-Custom"); len(values) != 2 || values[0] != "v1" || values[1] != "v2" {
		t.Fatalf("X-Custom values = %v, want [v1 v2]", values)
	}
}

func TestAppendProtocolEndpoint(t *testing.T) {
	endpoint, err := AppendProtocolEndpoint("https://api.openai.com", ai_protocols.OpenAIChat)
	if err != nil {
		t.Fatalf("AppendProtocolEndpoint() error = %v", err)
	}
	if endpoint != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("endpoint = %q", endpoint)
	}

	endpoint, err = AppendProtocolEndpoint("https://api.openai.com/v1", ai_protocols.OpenAIChat)
	if err != nil {
		t.Fatalf("AppendProtocolEndpoint(/v1) error = %v", err)
	}
	if endpoint != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("endpoint = %q", endpoint)
	}

	endpoint, err = AppendProtocolEndpoint("https://gateway.example/custom/path", ai_protocols.OpenAIChat)
	if err != nil {
		t.Fatalf("AppendProtocolEndpoint(custom) error = %v", err)
	}
	if endpoint != "https://gateway.example/custom/path" {
		t.Fatalf("endpoint = %q, want unchanged custom path", endpoint)
	}
}

func TestAppendBedrockEndpoint(t *testing.T) {
	endpoint, err := AppendBedrockEndpoint("https://bedrock-runtime.us-east-1.amazonaws.com", "claude-3", false)
	if err != nil {
		t.Fatalf("AppendBedrockEndpoint() error = %v", err)
	}
	if endpoint != "https://bedrock-runtime.us-east-1.amazonaws.com/model/claude-3/converse" {
		t.Fatalf("endpoint = %q", endpoint)
	}

	endpoint, err = AppendBedrockEndpoint("https://bedrock-runtime.us-east-1.amazonaws.com", "claude 3", true)
	if err != nil {
		t.Fatalf("AppendBedrockEndpoint(stream) error = %v", err)
	}
	if endpoint != "https://bedrock-runtime.us-east-1.amazonaws.com/model/claude%203/converse-stream" {
		t.Fatalf("endpoint = %q", endpoint)
	}

	if _, err := AppendBedrockEndpoint("https://bedrock.example", "", false); err == nil {
		t.Fatal("AppendBedrockEndpoint(empty model) error = nil, want error")
	}
}

func TestProviderUsesOpenAIChat(t *testing.T) {
	for _, provider := range []string{"openai", "deepseek", "openai-compatible", "azure-openai", "gemini", "vertex-ai"} {
		if !ProviderUsesOpenAIChat(provider) {
			t.Fatalf("ProviderUsesOpenAIChat(%s) = false, want true", provider)
		}
	}
	for _, provider := range []string{"bedrock", "anthropic", "unknown"} {
		if ProviderUsesOpenAIChat(provider) {
			t.Fatalf("ProviderUsesOpenAIChat(%s) = true, want false", provider)
		}
	}
}
