package ai_protocols

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestConvertAnthropicHeadersToOpenAI(t *testing.T) {
	headers := http.Header{
		"X-Api-Key":         []string{"secret"},
		"Anthropic-Version": []string{"2023-06-01"},
		"X-Stainless-Retry": []string{"0"},
	}
	ConvertAnthropicHeadersToOpenAI(headers)
	if headers.Get("Authorization") != "Bearer secret" || headers.Get("X-Api-Key") != "" ||
		headers.Get("Anthropic-Version") != "" || headers.Get("X-Stainless-Retry") != "" {
		t.Fatalf("converted headers = %#v", headers)
	}
}

func TestConvertAnthropicMessagesToOpenAI(t *testing.T) {
	converted, toolNames, err := ConvertAnthropicMessagesToOpenAI([]byte(`{
	  "model":"claude-client",
	  "system":[{"type":"text","text":"be concise"}],
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]},
	    {"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"lookup.weather","input":{"city":"SZ"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"sunny"}]}
	  ],
	  "max_tokens":128,
	  "stream":true,
	  "tool_choice":{"type":"tool","name":"lookup.weather","disable_parallel_tool_use":true},
	  "tools":[{"name":"lookup.weather","description":"lookup","input_schema":{"type":"object"}}]
	}`))
	if err != nil {
		t.Fatalf("ConvertAnthropicMessagesToOpenAI() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	if body["max_completion_tokens"] != float64(128) || body["max_tokens"] != nil ||
		body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("converted request options = %#v", body)
	}
	messages := body["messages"].([]any)
	if len(messages) != 4 || messages[0].(map[string]any)["role"] != "system" ||
		messages[3].(map[string]any)["role"] != "tool" {
		t.Fatalf("converted messages = %#v", messages)
	}
	tools := body["tools"].([]any)
	toolName := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if toolName != "lookup_weather" || toolNames[toolName] != "lookup.weather" {
		t.Fatalf("tool name = %q, map = %#v", toolName, toolNames)
	}
	choice := body["tool_choice"].(map[string]any)["function"].(map[string]any)["name"]
	if choice != "lookup_weather" || body["parallel_tool_calls"] != false {
		t.Fatalf("tool choice = %#v, parallel = %#v", choice, body["parallel_tool_calls"])
	}
}

func TestConvertOpenAIChatToAnthropic(t *testing.T) {
	converted, err := ConvertOpenAIChatToAnthropic([]byte(`{
	  "id":"chat-1","model":"provider-model",
	  "choices":[{"finish_reason":"tool_calls","message":{"reasoning_content":"think","content":"answer","tool_calls":[{"id":"call-1","function":{"name":"lookup_weather","arguments":"{\"city\":\"SZ\"}"}}]}}],
	  "usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}
	}`), "client-model", map[string]string{"lookup_weather": "lookup.weather"})
	if err != nil {
		t.Fatalf("ConvertOpenAIChatToAnthropic() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode converted response: %v", err)
	}
	content := body["content"].([]any)
	tool := content[2].(map[string]any)
	usage := body["usage"].(map[string]any)
	if body["type"] != "message" || body["model"] != "client-model" || body["stop_reason"] != "tool_use" ||
		tool["name"] != "lookup.weather" || usage["input_tokens"] != float64(7) ||
		usage["cache_read_input_tokens"] != float64(3) {
		t.Fatalf("converted response = %#v", body)
	}
}

func TestConvertAnthropicMessagesRejectsMissingMessages(t *testing.T) {
	_, _, err := ConvertAnthropicMessagesToOpenAI([]byte(`{"model":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "missing messages") {
		t.Fatalf("error = %v, want missing messages", err)
	}
}
