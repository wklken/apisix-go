package base

import (
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

// ResponseModeMask describes the response modes a plugin can select after its
// configuration has been initialized. A mask is used because a plugin may
// support both bounded and streaming responses while remaining incompatible
// with hijacking.
type ResponseModeMask uint8

const (
	ResponseModeNone      ResponseModeMask = 0
	ResponseModeBounded   ResponseModeMask = 1 << 0
	ResponseModeStreaming ResponseModeMask = 1 << 1
	ResponseModeHijack    ResponseModeMask = 1 << 2
)

type ResponseModeDescriptor struct {
	Modes ResponseModeMask
}

// ResponseModeDescriber is implemented by config-aware plugins whose mode
// cannot be determined from their factory identity alone.
type ResponseModeDescriber interface {
	DescribeResponseMode() (ResponseModeDescriptor, error)
}

// RequestResponseMode is the one concrete response path selected after all
// request phases have prepared request-local protocol state and before the
// first terminal response byte is written.
type RequestResponseMode uint8

const (
	RequestResponseModeBounded RequestResponseMode = iota + 1
	RequestResponseModeStreaming
)

// RequestResponseModeSelector is required when one binding declares both a
// bounded and streaming response callback. Selection is request-local; a
// generation must never execute both callbacks for one response.
type RequestResponseModeSelector interface {
	SelectResponseMode(*http.Request) RequestResponseMode
}

// StreamingResponseState is the mutable response metadata handed to a
// streaming header phase. Trailer is kept separate from Header so a wrapper
// can preserve trailer declarations without treating them as ordinary fields.
type StreamingResponseState struct {
	Status  int
	Header  http.Header
	Trailer http.Header
}

func CloneStreamingResponseState(state StreamingResponseState) StreamingResponseState {
	return StreamingResponseState{
		Status:  state.Status,
		Header:  cloneHeader(state.Header),
		Trailer: cloneHeader(state.Trailer),
	}
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	return header.Clone()
}

type StreamingHeaderFilterPlugin interface {
	RunStreamingHeaderFilter(*http.Request, *StreamingResponseState) error
}

type StreamingBodyFilterPlugin interface {
	WrapStreamingResponse(http.ResponseWriter, *http.Request) (http.ResponseWriter, error)
}

// StreamingResponseFinalizer is optional lifecycle ownership for wrappers
// that retain encoder or connection state. The executor invokes it exactly
// once with nil on normal completion or the terminal error/panic.
type StreamingResponseFinalizer interface {
	FinishStreamingResponse(error) error
}

type ProtocolDisposition uint8

const (
	ProtocolResponded ProtocolDisposition = iota + 1
	ProtocolHijacked
)

// ExclusiveProtocolTerminal owns one protocol response. The continuation is
// supplied so protocol translators can frame a normal upstream response while
// terminal-only owners can ignore it. The source must be selected before the
// owner writes, flushes, or hijacks.
type ExclusiveProtocolTerminal interface {
	RunExclusiveProtocol(
		http.ResponseWriter,
		*http.Request,
		http.Handler,
	) (ProtocolDisposition, *http.Request, apisixctx.ResponseSource, error)
}
