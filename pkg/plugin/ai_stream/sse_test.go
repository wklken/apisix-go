package ai_stream

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

func TestForwardSSEMergesAnthropicUsageAndPreservesWireBody(t *testing.T) {
	body := "event: message_start\n" +
		"data: {\"message\":{\"model\":\"claude\",\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rr := httptest.NewRecorder()

	usage, err := ForwardSSE(rr, strings.NewReader(body), ai_protocols.AnthropicMessages, 0)
	if err != nil {
		t.Fatalf("ForwardSSE() error = %v", err)
	}
	if rr.Body.String() != body {
		t.Fatalf("forwarded body = %q, want exact input", rr.Body.String())
	}
	if usage.Model != "claude" || usage.PromptTokens != 7 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestForwardSSEResponsesFinalUsage(t *testing.T) {
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-4.1\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n"
	rr := httptest.NewRecorder()

	usage, err := ForwardSSE(rr, strings.NewReader(body), ai_protocols.OpenAIResponses, 0)
	if err != nil {
		t.Fatalf("ForwardSSE() error = %v", err)
	}
	if usage.Model != "gpt-4.1" || usage.PromptTokens != 5 || usage.CompletionTokens != 2 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestForwardSSEEnforcesByteLimit(t *testing.T) {
	rr := httptest.NewRecorder()
	if _, err := ForwardSSE(rr, strings.NewReader("data: 12345\n\n"), ai_protocols.OpenAIChat, 5); err == nil {
		t.Fatal("ForwardSSE() error = nil, want byte limit error")
	}
}
