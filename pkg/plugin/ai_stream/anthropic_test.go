package ai_stream

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
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

func TestForwardOpenAIAsAnthropicSSEToolFragmentsSetPresenceNotCount(t *testing.T) {
	body := "data: {\"id\":\"chat-1\",\"model\":\"gpt-4\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"lookup_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	rr := httptest.NewRecorder()

	usage, err := ForwardOpenAIAsAnthropicSSE(rr, strings.NewReader(body), 0, nil)
	if err != nil {
		t.Fatalf("ForwardOpenAIAsAnthropicSSE() error = %v", err)
	}
	if !usage.HasToolCalls || usage.ToolCalls != 0 {
		t.Fatalf("usage = %#v, want HasToolCalls true and ToolCalls 0", usage)
	}
}

func TestForwardOpenAIAsAnthropicSSEConvertsAndWarnsOnError(t *testing.T) {
	body := "data: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n" +
		"data: [DONE]\n\n"
	rr := httptest.NewRecorder()
	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("ai-stream-anthropic-error-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "overloaded_error") {
			entries <- entry
		}
	})
	defer stop()

	_, err := ForwardOpenAIAsAnthropicSSE(rr, strings.NewReader(body), 0, nil)
	if err != nil {
		t.Fatalf("ForwardOpenAIAsAnthropicSSE() error = %v", err)
	}
	output := rr.Body.String()
	for _, expected := range []string{
		"event: error", `"type":"error"`, `"type":"overloaded_error"`, `"message":"Overloaded"`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("converted error stream missing %q:\n%s", expected, output)
		}
	}
	select {
	case entry := <-entries:
		if entry.Level != "WARN" || entry.Message !=
			"Anthropic SSE error: type=overloaded_error, message=Overloaded" {
			t.Fatalf("warning entry = %#v", entry)
		}
	case <-time.After(time.Second):
		t.Fatal("Anthropic SSE error was not logged at warning level")
	}
}

func TestForwardOpenAIAsAnthropicSSEReturnsErrorWithoutWritingForMismatchedFormat(t *testing.T) {
	body := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"type\":\"message\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: message_stop\n" +
		"data: {}\n\n"
	rr := httptest.NewRecorder()

	_, err := ForwardOpenAIAsAnthropicSSE(rr, strings.NewReader(body), 0, nil)
	if err == nil || !strings.Contains(err.Error(), "streaming response completed without producing any output") {
		t.Fatalf("ForwardOpenAIAsAnthropicSSE() error = %v, want no-output error", err)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("mismatched stream output = %q, want empty", rr.Body.String())
	}
}
