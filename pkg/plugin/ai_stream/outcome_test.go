package ai_stream

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

func TestForwardSSERejectsMalformedDataAfterFirstEvent(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n" +
		"data: {malformed\n\n"
	response := httptest.NewRecorder()

	_, err := ForwardSSE(response, strings.NewReader(body), ai_protocols.OpenAIChat, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid SSE data") {
		t.Fatalf("ForwardSSE() error = %v, want malformed data error", err)
	}
	if strings.Contains(response.Body.String(), "malformed") || !strings.Contains(response.Body.String(), "first") {
		t.Fatalf("forwarded body = %q, want only validated first event", response.Body.String())
	}
}

func TestWriteTerminalErrorUsesProtocolFraming(t *testing.T) {
	sse := httptest.NewRecorder()
	if err := WriteTerminalError(sse, StreamTransportSSE); err != nil {
		t.Fatalf("WriteTerminalError(SSE) error = %v", err)
	}
	if got := sse.Body.String(); !strings.Contains(got, "event: error") ||
		!strings.Contains(got, "upstream_stream_error") {
		t.Fatalf("SSE terminal event = %q", got)
	}

	aws := httptest.NewRecorder()
	if err := WriteTerminalError(aws, StreamTransportAWSEventStream); err != nil {
		t.Fatalf("WriteTerminalError(AWS) error = %v", err)
	}
	message, err := eventstream.NewDecoder().Decode(bytes.NewReader(aws.Body.Bytes()), nil)
	if err != nil {
		t.Fatalf("decode AWS terminal event: %v", err)
	}
	if got := headerString(message.Headers, ":message-type"); got != "exception" {
		t.Fatalf("AWS message type = %q, want exception", got)
	}
	if !strings.Contains(string(message.Payload), "upstream stream terminated") {
		t.Fatalf("AWS terminal payload = %q", message.Payload)
	}
}

func TestRecordStreamOutcomeUsesFixedClassificationAndRequestState(t *testing.T) {
	request := apisixctx.WithRequestVars(httptest.NewRequest("GET", "/", nil))
	if got := RecordStreamOutcome(request, StreamTransportSSE, nil); got != StreamOutcomeSuccess {
		t.Fatalf("success outcome = %q", got)
	}
	if got := apisixctx.GetRequestVar(request, "$ai_stream_outcome"); got != string(StreamOutcomeSuccess) {
		t.Fatalf("request outcome = %#v", got)
	}
	if got := RecordStreamOutcome(request, StreamTransportSSE, errors.New("bad frame")); got != StreamOutcomeError {
		t.Fatalf("error outcome = %q", got)
	}
	canceled, cancel := context.WithCancel(request.Context())
	cancel()
	if got := RecordStreamOutcome(
		request.WithContext(canceled),
		StreamTransportSSE,
		context.Canceled,
	); got != StreamOutcomeCanceled {
		t.Fatalf("canceled outcome = %q", got)
	}
}
