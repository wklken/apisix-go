package ai_stream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

type failingWriter struct {
	writer   http.ResponseWriter
	writes   int
	failFrom int
}

func (w *failingWriter) Header() http.Header    { return w.writer.Header() }
func (w *failingWriter) WriteHeader(status int) { w.writer.WriteHeader(status) }
func (w *failingWriter) Write(body []byte) (int, error) {
	w.writes++
	if w.writes >= w.failFrom {
		return 0, errors.New("broken pipe")
	}
	return w.writer.Write(body)
}
func (w *failingWriter) Flush() {}

func TestForwardSSEMergesAnthropicUsageAndPreservesWireBody(t *testing.T) {
	body := "event: message_start\n" +
		"data: {\"message\":{\"model\":\"claude\",\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
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
	if usage.Model != "claude" || usage.PromptTokens != 7 || usage.CompletionTokens != 3 || usage.Text != "hello" {
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
	if usage.Model != "gpt-4.1" || usage.PromptTokens != 5 || usage.CompletionTokens != 2 || usage.Text != "hi" {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestForwardSSEStreamingToolFragmentsSetPresenceNotCount(t *testing.T) {
	body := "data: {\"id\":\"1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	rr := httptest.NewRecorder()

	usage, err := ForwardSSE(rr, strings.NewReader(body), ai_protocols.OpenAIChat, 0)
	if err != nil {
		t.Fatalf("ForwardSSE() error = %v", err)
	}
	if !usage.HasToolCalls || usage.ToolCalls != 0 {
		t.Fatalf("usage = %#v, want HasToolCalls true and ToolCalls 0", usage)
	}
}

func TestForwardSSEResponsesStreamingFunctionCallSetsPresenceNotCount(t *testing.T) {
	body := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"get_weather\",\"arguments\":\"{}\",\"call_id\":\"call_1\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"gpt-4o-mini\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"get_weather\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":20,\"output_tokens\":5,\"total_tokens\":25}}}\n\n"
	rr := httptest.NewRecorder()

	usage, err := ForwardSSE(rr, strings.NewReader(body), ai_protocols.OpenAIResponses, 0)
	if err != nil {
		t.Fatalf("ForwardSSE() error = %v", err)
	}
	if !usage.HasToolCalls || usage.ToolCalls != 0 {
		t.Fatalf("usage = %#v, want HasToolCalls true and ToolCalls 0", usage)
	}
}

func TestForwardSSEAnthropicToolUseSetsPresenceNotCount(t *testing.T) {
	body := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	rr := httptest.NewRecorder()

	usage, err := ForwardSSE(rr, strings.NewReader(body), ai_protocols.AnthropicMessages, 0)
	if err != nil {
		t.Fatalf("ForwardSSE() error = %v", err)
	}
	if !usage.HasToolCalls || usage.ToolCalls != 0 {
		t.Fatalf("usage = %#v, want HasToolCalls true and ToolCalls 0", usage)
	}
}

func TestForwardSSEEnforcesByteLimit(t *testing.T) {
	rr := httptest.NewRecorder()
	if _, err := ForwardSSE(rr, strings.NewReader("data: 12345\n\n"), ai_protocols.OpenAIChat, 5); err == nil {
		t.Fatal("ForwardSSE() error = nil, want byte limit error")
	}
}

func TestForwardSSEIdentifiesClientDisconnectOnWriteFailure(t *testing.T) {
	rr := httptest.NewRecorder()
	failing := &failingWriter{writer: rr, failFrom: 1}
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"

	_, err := ForwardSSE(failing, strings.NewReader(body), ai_protocols.OpenAIChat, 0)
	if err == nil || !errors.Is(err, ErrClientDisconnected) {
		t.Fatalf("ForwardSSE() error = %v, want ErrClientDisconnected", err)
	}
}

func TestForwardAnthropicSSEIdentifiesClientDisconnectOnWriteFailure(t *testing.T) {
	rr := httptest.NewRecorder()
	failing := &failingWriter{writer: rr, failFrom: 1}
	body := "data: {\"id\":\"chat-1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	_, err := ForwardOpenAIAsAnthropicSSE(failing, strings.NewReader(body), 0, nil)
	if err == nil || !errors.Is(err, ErrClientDisconnected) {
		t.Fatalf("ForwardOpenAIAsAnthropicSSE() error = %v, want ErrClientDisconnected", err)
	}
}

func TestForwardAWSEventStreamIdentifiesClientDisconnectOnWriteFailure(t *testing.T) {
	rr := httptest.NewRecorder()
	failing := &failingWriter{writer: rr, failFrom: 1}
	payload := encodeTestAWSEventStreamMessage(t, "test", "")
	body := strings.NewReader(string(payload))

	_, err := ForwardAWSEventStream(failing, body, 0)
	if err == nil || !errors.Is(err, ErrClientDisconnected) {
		t.Fatalf("ForwardAWSEventStream() error = %v, want ErrClientDisconnected", err)
	}
}

func encodeTestAWSEventStreamMessage(t *testing.T, name string, payload string) []byte {
	t.Helper()
	var headers bytes.Buffer
	headers.WriteByte(byte(len(name)))
	headers.WriteString(name)
	headers.WriteByte(7)
	_ = binary.Write(&headers, binary.BigEndian, uint16(len(payload)))
	headers.WriteString(payload)
	totalLength := 16 + headers.Len() + len(payload)
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(headers.Len()))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headers.Bytes()...)
	frame = append(frame, payload...)
	messageCRC := crc32.ChecksumIEEE(frame)
	return append(frame, byte(messageCRC>>24), byte(messageCRC>>16), byte(messageCRC>>8), byte(messageCRC))
}

var _ io.Writer = (*failingWriter)(nil)
