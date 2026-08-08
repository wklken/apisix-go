package ai_protocols

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestAppendProtocolEndpoint(t *testing.T) {
	endpoint, err := AppendProtocolEndpoint("https://api.openai.com", OpenAIChat)
	if err != nil {
		t.Fatalf("AppendProtocolEndpoint() error = %v", err)
	}
	if endpoint != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("endpoint = %q", endpoint)
	}

	endpoint, err = AppendProtocolEndpoint("https://api.openai.com/v1", OpenAIChat)
	if err != nil {
		t.Fatalf("AppendProtocolEndpoint(/v1) error = %v", err)
	}
	if endpoint != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("endpoint = %q", endpoint)
	}

	endpoint, err = AppendProtocolEndpoint("https://gateway.example/custom/path", OpenAIChat)
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

func TestStringValue(t *testing.T) {
	if got := StringValue("text"); got != "text" {
		t.Fatalf("StringValue(string) = %q", got)
	}
	for _, value := range []any{42, nil, []any{"x"}, map[string]any{"a": 1}} {
		if got := StringValue(value); got != "" {
			t.Fatalf("StringValue(%#v) = %q, want empty", value, got)
		}
	}
}

func TestNumericUsage(t *testing.T) {
	tests := []struct {
		name  string
		value any
		round bool
		want  int64
	}{
		{name: "int", value: int(7), round: true, want: 7},
		{name: "int64", value: int64(8), round: false, want: 8},
		{name: "float rounded", value: 3.6, round: true, want: 4},
		{name: "float truncated", value: 3.6, round: false, want: 3},
		{name: "float half", value: 2.5, round: true, want: 3},
		{name: "float half truncated", value: 2.5, round: false, want: 2},
		{name: "negative float", value: -1.2, round: true, want: -1},
		{name: "string sentinel", value: "9", round: true, want: -1},
		{name: "nil sentinel", value: nil, round: true, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NumericUsage(tt.value, tt.round); got != tt.want {
				t.Fatalf("NumericUsage(%#v, %t) = %d, want %d", tt.value, tt.round, got, tt.want)
			}
		})
	}
}

func TestRegisterLLMRequestVarsPublishesSharedVariables(t *testing.T) {
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	requestDocument, err := DecodeDocument([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("DecodeDocument() error = %v", err)
	}
	responseDocument, err := DecodeDocument([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	if err != nil {
		t.Fatalf("DecodeDocument() error = %v", err)
	}

	RegisterLLMRequestVars(req, requestDocument, OpenAIChat, responseDocument)

	for name, want := range map[string]any{
		"$request_type":          OpenAIChat.RequestType,
		"$request_llm_model":     "gpt-4o",
		"$llm_prompt_tokens":     int64(10),
		"$llm_completion_tokens": int64(5),
		"$llm_raw_usage":         map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(5)},
	} {
		got := apisixctx.GetRequestVar(req, name)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("variable %s = %v, want %v", name, got, want)
		}
	}
	if got := apisixctx.GetRequestVar(req, "$ai_token_usage"); got == nil {
		t.Fatal("$ai_token_usage not published")
	}
}
