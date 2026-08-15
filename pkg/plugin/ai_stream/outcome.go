package ai_stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
)

type StreamTransport string

const (
	StreamTransportSSE            StreamTransport = "sse"
	StreamTransportAWSEventStream StreamTransport = "aws_eventstream"
)

type StreamOutcome string

const (
	StreamOutcomeSuccess  StreamOutcome = "success"
	StreamOutcomeError    StreamOutcome = "error"
	StreamOutcomeCanceled StreamOutcome = "canceled"
)

const terminalErrorMessage = "upstream stream terminated"

func RecordStreamOutcome(r *http.Request, transport StreamTransport, streamErr error) StreamOutcome {
	outcome := StreamOutcomeSuccess
	if streamErr != nil {
		outcome = StreamOutcomeError
		if errors.Is(streamErr, ErrClientDisconnected) || errors.Is(streamErr, context.Canceled) ||
			errors.Is(streamErr, context.DeadlineExceeded) || r != nil && r.Context().Err() != nil {
			outcome = StreamOutcomeCanceled
		}
	}
	if r != nil {
		apisixctx.RegisterRequestVar(r, "$ai_stream_transport", string(transport))
		apisixctx.RegisterRequestVar(r, "$ai_stream_outcome", string(outcome))
	}
	metrics.RecordAIStreamOutcome(string(transport), string(outcome))
	return outcome
}

func WriteTerminalError(w io.Writer, transport StreamTransport) error {
	switch transport {
	case StreamTransportSSE:
		_, err := io.WriteString(
			w,
			"event: error\ndata: {\"error\":{\"type\":\"upstream_stream_error\",\"message\":\""+
				terminalErrorMessage+"\"}}\n\n",
		)
		return err
	case StreamTransportAWSEventStream:
		return eventstream.NewEncoder().Encode(w, eventstream.Message{
			Headers: eventstream.Headers{
				{Name: ":message-type", Value: eventstream.StringValue("exception")},
				{Name: ":exception-type", Value: eventstream.StringValue("apisixUpstreamStreamError")},
				{Name: ":content-type", Value: eventstream.StringValue("application/json")},
			},
			Payload: []byte(`{"message":"` + terminalErrorMessage + `"}`),
		})
	default:
		return fmt.Errorf("unknown AI stream transport %q", transport)
	}
}
