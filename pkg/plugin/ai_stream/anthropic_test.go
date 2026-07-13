package ai_stream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForwardOpenAIAsAnthropicSSEDefersStopUntilUsage(t *testing.T) {
	body := "data: {\"id\":\"chat-1\",\"model\":\"gpt-4\",\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":1}}}\n\n" +
		"data: [DONE]\n\n"
	rr := httptest.NewRecorder()

	usage, err := ForwardOpenAIAsAnthropicSSE(rr, strings.NewReader(body), 0, nil)
	if err != nil {
		t.Fatalf("ForwardOpenAIAsAnthropicSSE() error = %v", err)
	}
	output := rr.Body.String()
	for _, expected := range []string{
		"event: message_start", "event: content_block_start", `"type":"thinking_delta"`,
		`"type":"text_delta"`, "event: message_delta", `"input_tokens":4`,
		`"cache_read_input_tokens":1`, "event: message_stop",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("converted stream missing %q:\n%s", expected, output)
		}
	}
	if strings.Index(output, "event: message_delta") > strings.Index(output, "event: message_stop") {
		t.Fatalf("message_delta must precede message_stop:\n%s", output)
	}
	if usage.Model != "gpt-4" || usage.PromptTokens != 5 || usage.CompletionTokens != 2 || usage.Text != "hello" {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestForwardOpenAIAsAnthropicSSEConvertsToolCallAndRestoresName(t *testing.T) {
	body := "data: {\"id\":\"chat-1\",\"model\":\"gpt-4\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"lookup_weather\",\"arguments\":\"{\\\"city\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"SZ\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	rr := httptest.NewRecorder()

	_, err := ForwardOpenAIAsAnthropicSSE(
		rr,
		strings.NewReader(body),
		0,
		map[string]string{"lookup_weather": "lookup.weather"},
	)
	if err != nil {
		t.Fatalf("ForwardOpenAIAsAnthropicSSE() error = %v", err)
	}
	output := rr.Body.String()
	for _, expected := range []string{
		`"name":"lookup.weather"`, `"type":"input_json_delta"`, `"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("converted stream missing %q:\n%s", expected, output)
		}
	}
}
