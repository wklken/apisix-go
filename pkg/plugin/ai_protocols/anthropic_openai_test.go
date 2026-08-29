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

func TestConvertAnthropicResponseFormatStrictSchema(t *testing.T) {
	converted, _, err := ConvertAnthropicMessagesToOpenAI([]byte(`{
	  "model":"m","max_tokens":100,
	  "messages":[{"role":"user","content":"hi"}],
	  "output_format":{"type":"json_schema","schema":{
	    "type":"object",
	    "properties":{"a":{"type":"string"},"b":{"type":"object","properties":{"c":{"type":"string"}}}},
	    "required":["a"]
	  }}
	}`))
	if err != nil {
		t.Fatalf("ConvertAnthropicMessagesToOpenAI() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	format := body["response_format"].(map[string]any)
	jsonSchema := format["json_schema"].(map[string]any)
	if format["type"] != "json_schema" || jsonSchema["name"] != "structured_output" || jsonSchema["strict"] != true {
		t.Fatalf("response_format = %#v", format)
	}
	schema := jsonSchema["schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatalf("top-level additionalProperties = %#v, want false", schema["additionalProperties"])
	}
	required := schema["required"].([]any)
	if len(required) != 2 || required[0] != "a" || required[1] != "b" {
		t.Fatalf("strict required = %#v, want [a b]", required)
	}
	properties := schema["properties"].(map[string]any)
	if a := properties["a"].(map[string]any); a["additionalProperties"] != nil {
		t.Fatalf("string property a must not gain additionalProperties: %#v", a)
	}
	if b := properties["b"].(map[string]any); b["additionalProperties"] != false {
		t.Fatalf("nested object property b must gain additionalProperties: %#v", b)
	}
}

func TestConvertAnthropicMessagesMatchesAPISIX317OutputConfigResponseFormat(t *testing.T) {
	converted, _, err := ConvertAnthropicMessagesToOpenAI([]byte(`{
	  "model":"m","max_tokens":100,
	  "messages":[{"role":"user","content":"hi"}],
	  "output_config":{"type":"json_schema","json_schema":{"name":"response","schema":{"type":"object"}}}
	}`))
	if err != nil {
		t.Fatalf("ConvertAnthropicMessagesToOpenAI() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	format, ok := body["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("response_format = %#v, want APISIX 3.17 json_schema", body["response_format"])
	}
	jsonSchema, ok := format["json_schema"].(map[string]any)
	if !ok || jsonSchema["name"] != "response" {
		t.Fatalf("response_format.json_schema = %#v, want name response", format["json_schema"])
	}
	if _, leaked := body["output_config"]; leaked {
		t.Fatalf("output_config leaked into converted request: %#v", body)
	}
}

func TestConvertAnthropicMessagesMatchesAPISIX317JSONObjectOutputFormat(t *testing.T) {
	converted, _, err := ConvertAnthropicMessagesToOpenAI([]byte(`{
	  "model":"m","max_tokens":100,
	  "messages":[{"role":"user","content":"hi"}],
	  "output_format":{"type":"json_object"}
	}`))
	if err != nil {
		t.Fatalf("ConvertAnthropicMessagesToOpenAI() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	format, ok := body["response_format"].(map[string]any)
	if !ok || format["type"] != "json_object" {
		t.Fatalf("response_format = %#v, want APISIX 3.17 json_object", body["response_format"])
	}
	if _, leaked := body["output_format"]; leaked {
		t.Fatalf("output_format leaked into converted request: %#v", body)
	}
}

func TestConvertAnthropicMessagesMatchesAPISIX317ToolResultOrdering(t *testing.T) {
	converted, _, err := ConvertAnthropicMessagesToOpenAI([]byte(`{
	  "model":"m","max_tokens":100,
	  "messages":[{"role":"user","content":[
	    {"type":"text","text":"Here are the results:"},
	    {"type":"tool_result","tool_use_id":"call_1","content":"done"}
	  ]}]
	}`))
	if err != nil {
		t.Fatalf("ConvertAnthropicMessagesToOpenAI() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	messages := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want two messages", messages)
	}
	first := messages[0].(map[string]any)
	second := messages[1].(map[string]any)
	if first["role"] != "user" || first["content"] != "Here are the results:" {
		t.Fatalf("first message = %#v, want APISIX 3.17 user text", first)
	}
	if second["role"] != "tool" || second["tool_call_id"] != "call_1" {
		t.Fatalf("second message = %#v, want APISIX 3.17 tool result", second)
	}
}

func TestConvertAnthropicResponseFormatNonObjectSchemas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format string
		wantRF bool
	}{
		{"json_object maps in APISIX 3.17", `{"type":"json_object"}`, true},
		{"schema-less json_schema omitted", `{"type":"json_schema"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			converted, _, err := ConvertAnthropicMessagesToOpenAI([]byte(`{
			  "model":"m","max_tokens":100,
			  "messages":[{"role":"user","content":"hi"}],
			  "output_format":` + tc.format + `
			}`))
			if err != nil {
				t.Fatalf("ConvertAnthropicMessagesToOpenAI() error = %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(converted, &body); err != nil {
				t.Fatalf("decode converted request: %v", err)
			}
			_, hasRF := body["response_format"]
			if hasRF != tc.wantRF {
				t.Fatalf("response_format present = %v, want %v", hasRF, tc.wantRF)
			}
		})
	}
}

func TestConvertAnthropicMessageSingleTextPartExtractsText(t *testing.T) {
	converted, err := convertAnthropicMessage("assistant", []any{
		map[string]any{"type": "text", "text": "single text part"},
	})
	if err != nil {
		t.Fatalf("convertAnthropicMessage() error = %v", err)
	}
	if len(converted) != 1 {
		t.Fatalf("converted = %v, want one message", converted)
	}
	message := converted[0].(map[string]any)
	if got := message["content"]; got != "single text part" {
		t.Fatalf("content = %v, want extracted text", got)
	}
}

func TestConvertAnthropicMessageMultipleTextPartsKeepParts(t *testing.T) {
	converted, err := convertAnthropicMessage("user", []any{
		map[string]any{"type": "text", "text": "first"},
		map[string]any{"type": "text", "text": "second"},
	})
	if err != nil {
		t.Fatalf("convertAnthropicMessage() error = %v", err)
	}
	message := converted[0].(map[string]any)
	parts, ok := message["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %v, want two parts", message["content"])
	}
}
