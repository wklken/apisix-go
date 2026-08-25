package plugin

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
)

// StreamingResponseExecutor owns streaming header filters, body wrappers and
// the optional exclusive protocol owner for one generation. It is immutable;
// all per-request state lives in the returned handler/writer chain.
type StreamingResponseExecutor struct {
	bindings              []Binding
	terminals             []RouteTerminalCandidate
	hasStaticHeaderFilter bool
}

type streamingFinish struct {
	once        sync.Once
	result      streamingFinishResult
	closers     []streamingCloserEntry
	finalizers  []streamingFinalizerEntry
	compression *streamingCompression
}

type streamingCloserEntry struct {
	factory string
	phase   Phase
	closer  io.Closer
}

type streamingFinalizerEntry struct {
	factory   string
	phase     Phase
	finalizer base.StreamingResponseFinalizer
}

type streamingFinishResult struct {
	Err    error
	Panics []*PanicError
}

var errStreamingPanic = errors.New("streaming pipeline panicked")

type compressionOfferEntry struct {
	factory string
	phase   Phase
	plugin  CompressionOfferPlugin
	offer   compression.Offer
}

type streamingCompression struct {
	request *http.Request
	state   *compression.State
	offers  []compressionOfferEntry
}

type dynamicStreamingBindingsKey struct{}

func withDynamicStreamingBindings(r *http.Request, bindings []Binding) *http.Request {
	return r.WithContext(context.WithValue(
		r.Context(),
		dynamicStreamingBindingsKey{},
		cloneBindings(bindings),
	))
}

func dynamicStreamingBindings(r *http.Request) []Binding {
	if r == nil {
		return nil
	}
	bindings, _ := r.Context().Value(dynamicStreamingBindingsKey{}).([]Binding)
	return cloneBindings(bindings)
}

type streamingWriteResult struct {
	n   int
	err error
}

type streamingReadFromResult struct {
	n   int64
	err error
}

type streamingHijackResult struct {
	conn net.Conn
	rw   *bufio.ReadWriter
	err  error
}

type streamingProtocolResult struct {
	disposition base.ProtocolDisposition
	request     *http.Request
	source      apisixctx.ResponseSource
}

type streamingWriterOwner struct {
	factory string
	phase   Phase
}

func streamingWriterValue[T any](owner *streamingWriterOwner, call func() T) (value T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if owner == nil {
			panic(downstreamPanic{value: recovered})
		}
		if downstream, ok := recovered.(downstreamPanic); ok {
			panic(downstream.value)
		}
		if recovered == http.ErrAbortHandler {
			panic(recovered)
		}
		if panicErr, ok := recovered.(*PanicError); ok {
			panic(panicErr)
		}
		panic(&PanicError{
			Factory: owner.factory,
			Phase:   owner.phase,
			Value:   recovered,
			Stack:   debug.Stack(),
		})
	}()
	return call()
}

func guardStreamingValue[T any](
	factory string,
	phase Phase,
	call func() (T, error),
) (value T, err error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if downstream, ok := recovered.(downstreamPanic); ok {
			panic(downstream.value)
		}
		if recovered == http.ErrAbortHandler {
			panic(recovered)
		}
		if panicErr, ok := recovered.(*PanicError); ok {
			err = panicErr
			return
		}
		err = &PanicError{
			Factory: factory,
			Phase:   phase,
			Value:   recovered,
			Stack:   debug.Stack(),
		}
	}()
	return call()
}

func streamingWriterHooks(owner *streamingWriterOwner) httpsnoop.Hooks {
	return httpsnoop.Hooks{
		Header: func(header httpsnoop.HeaderFunc) httpsnoop.HeaderFunc {
			return func() http.Header { return streamingWriterValue(owner, header) }
		},
		WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				streamingWriterValue(owner, func() struct{} { writeHeader(status); return struct{}{} })
			}
		},
		Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				result := streamingWriterValue(owner, func() streamingWriteResult {
					n, err := write(body)
					return streamingWriteResult{n: n, err: err}
				})
				return result.n, result.err
			}
		},
		WriteString: func(writeString httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) {
				result := streamingWriterValue(owner, func() streamingWriteResult {
					n, err := writeString(value)
					return streamingWriteResult{n: n, err: err}
				})
				return result.n, result.err
			}
		},
		ReadFrom: func(readFrom httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) {
				result := streamingWriterValue(owner, func() streamingReadFromResult {
					n, err := readFrom(reader)
					return streamingReadFromResult{n: n, err: err}
				})
				return result.n, result.err
			}
		},
		Flush: func(flush httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				streamingWriterValue(owner, func() struct{} { flush(); return struct{}{} })
			}
		},
		FlushError: func(flush httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error { return streamingWriterValue(owner, flush) }
		},
		CloseNotify: func(closeNotify httpsnoop.CloseNotifyFunc) httpsnoop.CloseNotifyFunc {
			return func() <-chan bool { return streamingWriterValue(owner, closeNotify) }
		},
		Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) {
				result := streamingWriterValue(owner, func() streamingHijackResult {
					conn, rw, err := hijack()
					return streamingHijackResult{conn: conn, rw: rw, err: err}
				})
				return result.conn, result.rw, result.err
			}
		},
		Push: func(push httpsnoop.PushFunc) httpsnoop.PushFunc {
			return func(target string, options *http.PushOptions) error {
				return streamingWriterValue(owner, func() error { return push(target, options) })
			}
		},
		SetReadDeadline: func(set httpsnoop.SetReadDeadlineFunc) httpsnoop.SetReadDeadlineFunc {
			return func(deadline time.Time) error {
				return streamingWriterValue(owner, func() error { return set(deadline) })
			}
		},
		SetWriteDeadline: func(set httpsnoop.SetWriteDeadlineFunc) httpsnoop.SetWriteDeadlineFunc {
			return func(deadline time.Time) error {
				return streamingWriterValue(owner, func() error { return set(deadline) })
			}
		},
		EnableFullDuplex: func(enable httpsnoop.EnableFullDuplexFunc) httpsnoop.EnableFullDuplexFunc {
			return func() error { return streamingWriterValue(owner, enable) }
		},
	}
}

func protectStreamingDownstreamWriter(w http.ResponseWriter) http.ResponseWriter {
	return httpsnoop.Wrap(w, streamingWriterHooks(nil))
}

func guardStreamingOwnerWriter(factory string, phase Phase, w http.ResponseWriter) http.ResponseWriter {
	return httpsnoop.Wrap(w, streamingWriterHooks(&streamingWriterOwner{factory: factory, phase: phase}))
}

func guardStreamingFinishCall(factory string, phase Phase, call func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if panicErr, ok := recovered.(*PanicError); ok {
				err = panicErr
				return
			}
			err = &PanicError{
				Factory: factory,
				Phase:   phase,
				Value:   recovered,
				Stack:   debug.Stack(),
			}
		}
	}()
	return call()
}

func (f *streamingFinish) finish(cause error) streamingFinishResult {
	if f == nil {
		return streamingFinishResult{}
	}
	f.once.Do(func() {
		for _, entry := range slices.Backward(f.closers) {
			f.recordFinishError(guardStreamingFinishCall(entry.factory, entry.phase, entry.closer.Close))
		}
		for _, entry := range slices.Backward(f.finalizers) {
			f.recordFinishError(guardStreamingFinishCall(entry.factory, entry.phase, func() error {
				return entry.finalizer.FinishStreamingResponse(cause)
			}))
		}
	})
	return cloneStreamingFinishResult(f.result)
}

func (f *streamingFinish) recordFinishError(err error) {
	if err == nil {
		return
	}
	if panicErr, ok := err.(*PanicError); ok {
		f.result.Panics = append(f.result.Panics, cloneStreamingPanicError(panicErr))
		return
	}
	if f.result.Err == nil {
		f.result.Err = err
	}
}

func cloneStreamingFinishResult(result streamingFinishResult) streamingFinishResult {
	clone := streamingFinishResult{Err: result.Err}
	if len(result.Panics) == 0 {
		return clone
	}
	clone.Panics = make([]*PanicError, len(result.Panics))
	for index, panicErr := range result.Panics {
		clone.Panics[index] = cloneStreamingPanicError(panicErr)
	}
	return clone
}

func cloneStreamingPanicError(panicErr *PanicError) *PanicError {
	if panicErr == nil {
		return nil
	}
	clone := *panicErr
	clone.Stack = append([]byte(nil), panicErr.Stack...)
	return &clone
}

func streamingPanicError(err error) *PanicError {
	var panicErr *PanicError
	if errors.As(err, &panicErr) {
		return panicErr
	}
	return nil
}

func logStreamingFinishPanics(panics []*PanicError) {
	for _, panicErr := range panics {
		if panicErr == nil {
			continue
		}
		logger.Errorf(
			"additional streaming finish panic factory=%q phase=%q",
			sanitizeDiagnostic(panicErr.Factory),
			panicErr.Phase,
		)
	}
}

func panicFirstStreamingFinish(result streamingFinishResult) {
	if len(result.Panics) == 0 {
		return
	}
	logStreamingFinishPanics(result.Panics[1:])
	panic(result.Panics[0])
}

func NewStreamingResponseExecutor(bindings []Binding) (*StreamingResponseExecutor, error) {
	cloned, err := resolveBindingsForPlan(bindings)
	if err != nil {
		return nil, err
	}
	executor := &StreamingResponseExecutor{bindings: cloned}
	for index := range executor.bindings {
		binding := &executor.bindings[index]
		if binding.Plugin == nil {
			return nil, fmt.Errorf(
				"streaming binding has nil plugin (factory=%q resource=%s/%s)",
				binding.Descriptor.Factory, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
		capability := binding.Descriptor.responseCapability
		if capability.HeaderFilter && (!capability.SeparateSubsystem || binding.Descriptor.Factory != "mqtt-proxy") {
			executor.hasStaticHeaderFilter = true
		}
		if capability.ExclusiveProtocol != ProtocolNone {
			terminal, ok := binding.Plugin.(base.ExclusiveProtocolTerminal)
			if !ok {
				continue // route-owned protocol terminals are supplied separately
			}
			executor.terminals = append(executor.terminals, RouteTerminalCandidate{
				Identity: binding.Descriptor.Factory, Scope: binding.Scope, Priority: binding.Priority,
				Provenance: binding.Provenance, Protocol: capability.ExclusiveProtocol, Terminal: terminal,
			})
		}
	}
	if err := validateExclusiveTerminalCandidates(executor.terminals); err != nil {
		return nil, err
	}
	return executor, nil
}

func (e *StreamingResponseExecutor) WithRouteTerminals(
	terminals []RouteTerminalCandidate,
) (*StreamingResponseExecutor, error) {
	if e == nil {
		return nil, fmt.Errorf("streaming response executor is nil")
	}
	clone := *e
	clone.bindings = cloneBindings(e.bindings)
	clone.terminals = append([]RouteTerminalCandidate(nil), terminals...)
	if err := validateExclusiveTerminalCandidates(clone.terminals); err != nil {
		return nil, err
	}
	return &clone, nil
}

func validateExclusiveTerminalCandidates(terminals []RouteTerminalCandidate) error {
	for _, candidate := range terminals {
		if candidate.Terminal == nil {
			return fmt.Errorf(
				"exclusive protocol identity=%q resource=%s/%s has nil terminal owner",
				candidate.Identity, candidate.Provenance.Kind, candidate.Provenance.ID,
			)
		}
	}
	if len(terminals) > 1 {
		left, right := terminals[0], terminals[1]
		return fmt.Errorf(
			"exclusive protocol identities %q (resource=%s/%s) and %q (resource=%s/%s) conflict",
			left.Identity, left.Provenance.Kind, left.Provenance.ID,
			right.Identity, right.Provenance.Kind, right.Provenance.ID,
		)
	}
	return nil
}

func (e *StreamingResponseExecutor) Bindings() []Binding {
	if e == nil {
		return nil
	}
	return cloneBindings(e.bindings)
}

func (e *StreamingResponseExecutor) wrapStreamingResponse(
	w http.ResponseWriter,
	r *http.Request,
) (http.ResponseWriter, *http.Request, *streamingFinish, error) {
	w = guardStreamingResponseSource(w, r)
	request, negotiation, err := e.registerCompressionOffers(r)
	if err != nil {
		return nil, r, nil, err
	}
	finish := &streamingFinish{compression: negotiation}
	w = e.wrapStreamingHeaderFilters(w, r)
	inner := finish.wrapNegotiatedCompression(w, request)
	wrapped, err := e.wrapBody(inner, request, finish)
	return wrapped, request, finish, err
}

func guardStreamingResponseSource(w http.ResponseWriter, r *http.Request) http.ResponseWriter {
	ensureSource := func() {
		lifecycle := apisixctx.GetRequestLifecycle(r)
		if lifecycle != nil && lifecycle.ResponseSource() == apisixctx.ResponseSourceUnknown {
			panic(streamingSetupError{err: fmt.Errorf("streaming response source is unknown before commit")})
		}
	}
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) { ensureSource(); writeHeader(status) }
		},
		Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) { ensureSource(); return write(body) }
		},
		Flush: func(flush httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() { ensureSource(); flush() }
		},
		FlushError: func(flush httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error { ensureSource(); return flush() }
		},
		Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) { ensureSource(); return hijack() }
		},
	})
}

func (e *StreamingResponseExecutor) PostResolutionHook(
	r *http.Request,
	effective EffectiveBindingSet,
) (*http.Request, error) {
	if e == nil {
		return r, nil
	}
	bufferStreamingHeaders := hasConditionalTerminalEffective(effective)
	dynamicHeaders := make([]Binding, 0)
	for _, binding := range effective.all() {
		if binding.Provenance.Kind != ResourceConsumer && binding.Provenance.Kind != ResourceConsumerGroup {
			continue
		}
		capability, err := responseCapabilityForBinding(binding)
		if err != nil {
			return r, err
		}
		bufferStreamingHeader, err := bindingUsesBufferedStreamingHeaderFallback(
			binding,
			bufferStreamingHeaders,
		)
		if err != nil {
			return r, err
		}
		buffered, err := materializeResponseBindings(
			EffectiveBindingSet{merged: []Binding{binding}},
			bufferStreamingHeader,
		)
		if err != nil {
			return r, err
		}
		if capability.CompressionOffer || capability.BufferedBodyFilter || capability.StreamingBodyFilter ||
			capability.StreamingResponseOwner || capability.ExclusiveProtocol != ProtocolNone ||
			(len(buffered) > 0 && !bufferStreamingHeader) {
			return r, fmt.Errorf(
				"dynamic response identity=%q resource=%s/%s is not supported",
				binding.Descriptor.Factory,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
		if capability.HeaderFilter && !bufferStreamingHeader {
			dynamicHeaders = append(dynamicHeaders, binding)
		}
	}
	if len(dynamicHeaders) > 0 {
		return withDynamicStreamingBindings(r, dynamicHeaders), nil
	}
	return r, nil
}

func dynamicHeaderBindingsForEffective(effective EffectiveBindingSet) []Binding {
	bufferStreamingHeaders := hasConditionalTerminalEffective(effective)
	bindings := dynamicHeaderBindingsForPartition(nil, effective.global, bufferStreamingHeaders)
	return dynamicHeaderBindingsForPartition(bindings, effective.merged, bufferStreamingHeaders)
}

func dynamicHeaderBindingsForPartition(
	bindings []Binding,
	partition []Binding,
	bufferStreamingHeaders bool,
) []Binding {
	for _, binding := range partition {
		if binding.Provenance.Kind != ResourceConsumer && binding.Provenance.Kind != ResourceConsumerGroup {
			continue
		}
		capability, err := responseCapabilityForBinding(binding)
		if err != nil || !capability.HeaderFilter {
			continue
		}
		bufferStreamingHeader, err := bindingUsesBufferedStreamingHeaderFallback(
			binding,
			bufferStreamingHeaders,
		)
		if err != nil || bufferStreamingHeader {
			continue
		}
		bindings = append(bindings, binding)
	}
	return bindings
}

func bindingUsesBufferedStreamingHeaderFallback(binding Binding, enabled bool) (bool, error) {
	if !enabled {
		return false, nil
	}
	return bindingDeclaresStreamingHeader(binding)
}

func (e *StreamingResponseExecutor) runHeaderFilters(r *http.Request, state *base.StreamingResponseState) error {
	headerBindings := e.phaseBindingsFor(
		mergeStreamingBindings(e.bindings, dynamicStreamingBindings(r)),
		func(capability ResponseCapability) bool { return capability.HeaderFilter },
	)
	for _, binding := range headerBindings {
		plugin, ok := binding.Plugin.(base.StreamingHeaderFilterPlugin)
		if !ok {
			return fmt.Errorf(
				"factory %q declares streaming header filter without callback (resource=%s/%s)",
				binding.Descriptor.Factory, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
		if err := guardCall(binding.Descriptor.Factory, PhaseHeaderFilter, func() error {
			return plugin.RunStreamingHeaderFilter(r, state)
		}); err != nil {
			return fmt.Errorf("factory %q streaming header filter: %w", binding.Descriptor.Factory, err)
		}
	}
	return nil
}

func (e *StreamingResponseExecutor) wrapStreamingHeaderFilters(
	w http.ResponseWriter,
	r *http.Request,
) http.ResponseWriter {
	if !e.hasStreamingHeaderFilter(r) {
		return w
	}
	var applied atomic.Bool
	apply := func(status int) {
		if status >= 100 && status <= 199 && status != http.StatusSwitchingProtocols {
			return
		}
		if !applied.CompareAndSwap(false, true) {
			return
		}
		state := base.StreamingResponseState{
			Status: status, Header: w.Header().Clone(), Trailer: make(http.Header),
		}
		if err := e.runHeaderFilters(r, &state); err != nil {
			panic(streamingSetupError{err: err})
		}
		copyHeader(w.Header(), state.Header)
	}
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				apply(status)
				writeHeader(status)
			}
		},
		Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				apply(http.StatusOK)
				return write(body)
			}
		},
		Flush: func(flush httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				apply(http.StatusOK)
				flush()
			}
		},
		FlushError: func(flush httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error {
				apply(http.StatusOK)
				return flush()
			}
		},
		ReadFrom: func(readFrom httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) {
				apply(http.StatusOK)
				return readFrom(reader)
			}
		},
		WriteString: func(writeString httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) {
				apply(http.StatusOK)
				return writeString(value)
			}
		},
	})
}

func (e *StreamingResponseExecutor) hasStreamingHeaderFilter(r *http.Request) bool {
	if e != nil && e.hasStaticHeaderFilter {
		return true
	}
	if r == nil {
		return false
	}
	bindings, _ := r.Context().Value(dynamicStreamingBindingsKey{}).([]Binding)
	return len(bindings) > 0
}

func (e *StreamingResponseExecutor) wrapBody(
	w http.ResponseWriter,
	r *http.Request,
	finish *streamingFinish,
) (http.ResponseWriter, error) {
	current := http.ResponseWriter(w)
	bodyBindings := e.phaseBindings(func(capability ResponseCapability) bool { return capability.StreamingBodyFilter })
	for _, binding := range slices.Backward(bodyBindings) {
		capability, capabilityErr := responseCapabilityForBinding(binding)
		if capabilityErr != nil {
			return nil, capabilityErr
		}
		if capability.CompressionOffer {
			// Compression offers are negotiated once below. They must not also
			// run as independent body wrappers, which would create nested
			// encoders and freeze separate decisions.
			if _, ok := binding.Plugin.(CompressionOfferPlugin); ok {
				continue
			}
		}
		if isDualModeResponseBinding(binding, capability) {
			mode, modeErr := guardValue(
				binding.Descriptor.Factory,
				PhaseBodyFilter,
				func() (base.RequestResponseMode, error) {
					return binding.Plugin.(base.RequestResponseModeSelector).SelectResponseMode(r), nil
				},
			)
			if modeErr != nil {
				return nil, modeErr
			}
			if mode != base.RequestResponseModeStreaming {
				continue
			}
		}
		plugin, ok := binding.Plugin.(base.StreamingBodyFilterPlugin)
		if !ok {
			return nil, fmt.Errorf(
				"factory %q declares streaming body filter without callback (resource=%s/%s)",
				binding.Descriptor.Factory, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
		wrapped, err := guardStreamingValue(
			binding.Descriptor.Factory,
			PhaseBodyFilter,
			func() (http.ResponseWriter, error) {
				return plugin.WrapStreamingResponse(protectStreamingDownstreamWriter(current), r)
			},
		)
		if err != nil {
			_ = finish.finish(err)
			return nil, fmt.Errorf("factory %q streaming body wrapper: %w", binding.Descriptor.Factory, err)
		}
		if wrapped == nil {
			_ = finish.finish(nil)
			return nil, fmt.Errorf("factory %q streaming body wrapper returned nil", binding.Descriptor.Factory)
		}
		if closer, ok := wrapped.(io.Closer); ok {
			finish.closers = append(finish.closers, streamingCloserEntry{
				factory: binding.Descriptor.Factory, phase: PhaseBodyFilter, closer: closer,
			})
		}
		if finalizer, ok := wrapped.(base.StreamingResponseFinalizer); ok {
			finish.finalizers = append(finish.finalizers, streamingFinalizerEntry{
				factory: binding.Descriptor.Factory, phase: PhaseBodyFilter, finalizer: finalizer,
			})
		}
		current = guardStreamingOwnerWriter(binding.Descriptor.Factory, PhaseBodyFilter, wrapped)
	}
	return current, nil
}

func (f *streamingFinish) wrapNegotiatedCompression(
	w http.ResponseWriter,
	r *http.Request,
) http.ResponseWriter {
	if f == nil || f.compression == nil {
		return w
	}
	var selected http.ResponseWriter
	selectWriter := func(status int) http.ResponseWriter {
		if selected != nil {
			return selected
		}
		var err error
		selected, err = f.applyCompression(w, r, status)
		if err != nil {
			panic(streamingSetupError{err: err})
		}
		return selected
	}
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				if status < http.StatusOK && status != http.StatusSwitchingProtocols {
					writeHeader(status)
					return
				}
				selectWriter(status).WriteHeader(status)
			}
		},
		Write: func(_ httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) { return selectWriter(http.StatusOK).Write(body) }
		},
		WriteString: func(_ httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) { return io.WriteString(selectWriter(http.StatusOK), value) }
		},
		ReadFrom: func(_ httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) { return io.Copy(selectWriter(http.StatusOK), reader) }
		},
		Flush: func(_ httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				if err := http.NewResponseController(selectWriter(http.StatusOK)).Flush(); err != nil {
					panic(streamingSetupError{err: err})
				}
			}
		},
		FlushError: func(_ httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error { return http.NewResponseController(selectWriter(http.StatusOK)).Flush() }
		},
	})
}

type streamingSetupError struct{ err error }

func (e streamingSetupError) Error() string { return e.err.Error() }

func (e *StreamingResponseExecutor) registerCompressionOffers(
	r *http.Request,
) (*http.Request, *streamingCompression, error) {
	bindings := e.phaseBindings(func(capability ResponseCapability) bool { return capability.CompressionOffer })
	if len(bindings) == 0 {
		return r, nil, nil
	}
	request, state := compression.Register(r)
	if request == nil || state == nil {
		return r, nil, fmt.Errorf("compression state could not be initialized")
	}
	negotiation := &streamingCompression{request: request, state: state}
	allOffers := make([]compression.Offer, 0, len(bindings))
	for _, binding := range bindings {
		plugin, ok := binding.Plugin.(CompressionOfferPlugin)
		if !ok {
			return r, nil, fmt.Errorf(
				"factory %q declares compression offer without structural callback (resource=%s/%s)",
				binding.Descriptor.Factory, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
		offers, offerErr := guardValue(
			binding.Descriptor.Factory,
			PhaseBodyFilter,
			func() ([]compression.Offer, error) {
				return plugin.RegisterCompressionOffers(request, state), nil
			},
		)
		if offerErr != nil {
			return r, nil, offerErr
		}
		for _, offer := range offers {
			if offer.Coding == compression.Identity {
				continue
			}
			allOffers = append(allOffers, offer)
			negotiation.offers = append(negotiation.offers, compressionOfferEntry{
				factory: binding.Descriptor.Factory,
				phase:   PhaseBodyFilter,
				plugin:  plugin,
				offer:   offer,
			})
		}
	}
	if len(allOffers) == 0 {
		return r, nil, nil
	}
	request, state = compression.Register(request, allOffers...)
	negotiation.request = request
	negotiation.state = state
	return request, negotiation, nil
}

func (f *streamingFinish) applyCompression(
	w http.ResponseWriter,
	r *http.Request,
	status int,
) (http.ResponseWriter, error) {
	if f == nil || f.compression == nil || f.compression.state == nil {
		return w, nil
	}
	decision := f.compression.state.Decide(compression.ResponseMeta{
		Method: r.Method, Status: status, Header: w.Header().Clone(),
	})
	if decision.Vary {
		base.AppendVaryToken(w.Header(), "Accept-Encoding")
	}
	if decision.NotAcceptable {
		return newNotAcceptableResponseWriter(w), nil
	}
	if decision.Coding == compression.Identity {
		return w, nil
	}
	for _, entry := range f.compression.offers {
		if entry.offer.Coding != decision.Coding {
			continue
		}
		wrapped, err := guardStreamingValue(entry.factory, entry.phase, func() (http.ResponseWriter, error) {
			return entry.plugin.WrapCompression(
				protectStreamingDownstreamWriter(w),
				r,
				f.compression.state,
				decision,
			)
		})
		if err != nil {
			return nil, err
		}
		if wrapped == nil {
			return nil, fmt.Errorf("compression factory returned nil writer")
		}
		if closer, ok := wrapped.(io.Closer); ok {
			f.closers = append(f.closers, streamingCloserEntry{
				factory: entry.factory, phase: entry.phase, closer: closer,
			})
		}
		if finalizer, ok := wrapped.(base.StreamingResponseFinalizer); ok {
			f.finalizers = append(f.finalizers, streamingFinalizerEntry{
				factory: entry.factory, phase: entry.phase, finalizer: finalizer,
			})
		}
		return guardStreamingOwnerWriter(entry.factory, entry.phase, wrapped), nil
	}
	return w, nil
}

func newNotAcceptableResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	var committed atomic.Bool
	reject := func() {
		if !committed.CompareAndSwap(false, true) {
			return
		}
		base.InvalidateBodyDerivedHeaders(w.Header())
		w.WriteHeader(http.StatusNotAcceptable)
	}
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(int) { reject() }
		},
		Write: func(httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				reject()
				return len(body), nil
			}
		},
		WriteString: func(httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) {
				reject()
				return len(value), nil
			}
		},
		ReadFrom: func(httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) {
				reject()
				return io.Copy(io.Discard, reader)
			}
		},
		Flush: func(httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				reject()
				_ = http.NewResponseController(w).Flush()
			}
		},
		FlushError: func(httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error {
				reject()
				return http.NewResponseController(w).Flush()
			}
		},
	})
}

// CommitResponse composes Plan 16 header/body adapters after bounded Plan 15
// transformations and stores have produced the final canonical state.
func (e *StreamingResponseExecutor) CommitResponse(
	w http.ResponseWriter,
	r *http.Request,
	state *base.ResponseState,
	commit BaseCommit,
) error {
	if e == nil {
		commit(w, state)
		return nil
	}
	streamingState := base.StreamingResponseState{
		Status: state.Status, Header: state.Header.Clone(), Trailer: make(http.Header),
	}
	if err := e.runHeaderFilters(r, &streamingState); err != nil {
		return err
	}
	state.Status = streamingState.Status
	state.Header = streamingState.Header
	request, negotiation, err := e.registerCompressionOffers(r)
	if err != nil {
		return err
	}
	finish := &streamingFinish{compression: negotiation}
	inner, err := finish.applyCompression(w, request, state.Status)
	if err != nil {
		result := finish.finish(err)
		if panicErr := streamingPanicError(err); panicErr != nil {
			logStreamingFinishPanics(result.Panics)
			panic(panicErr)
		}
		panicFirstStreamingFinish(result)
		return err
	}
	wrapped, err := e.wrapBody(inner, request, finish)
	if err != nil {
		result := finish.finish(err)
		if panicErr := streamingPanicError(err); panicErr != nil {
			logStreamingFinishPanics(result.Panics)
			panic(panicErr)
		}
		panicFirstStreamingFinish(result)
		return err
	}
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		commit(wrapped, state)
	}()
	if panicValue != nil {
		result := finish.finish(errStreamingPanic)
		logStreamingFinishPanics(result.Panics)
		panic(panicValue)
	}
	result := finish.finish(nil)
	panicFirstStreamingFinish(result)
	if result.Err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (e *StreamingResponseExecutor) phaseBindings(want func(ResponseCapability) bool) []Binding {
	return e.phaseBindingsFor(e.bindings, want)
}

func (e *StreamingResponseExecutor) phaseBindingsFor(
	bindings []Binding,
	want func(ResponseCapability) bool,
) []Binding {
	selected := make([]Binding, 0, len(bindings))
	for _, binding := range bindings {
		capability, err := responseCapabilityForBinding(binding)
		if err != nil || !want(capability) ||
			capability.SeparateSubsystem && binding.Descriptor.Factory == "mqtt-proxy" {
			continue
		}
		selected = append(selected, binding)
	}
	slices.SortStableFunc(selected, compareBindings)
	return selected
}

func mergeStreamingBindings(static, dynamic []Binding) []Binding {
	merged := cloneBindings(static)
	indexes := make(map[string]int, len(merged))
	for index, binding := range merged {
		if binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal || binding.Descriptor.Factory == "" {
			continue
		}
		indexes[binding.Descriptor.Factory] = index
	}
	for _, binding := range dynamic {
		if index, ok := indexes[binding.Descriptor.Factory]; ok {
			merged[index] = binding
			continue
		}
		if binding.Descriptor.Factory != "" {
			indexes[binding.Descriptor.Factory] = len(merged)
		}
		merged = append(merged, binding)
	}
	return merged
}

func compareBindings(a, b Binding) int {
	if a.Scope != b.Scope {
		return cmp.Compare(a.Scope, b.Scope)
	}
	if phase := compareDescriptorPhase(a.Descriptor, b.Descriptor); phase != 0 {
		return phase
	}
	if priority := cmp.Compare(b.Priority, a.Priority); priority != 0 {
		return priority
	}
	if factory := cmp.Compare(a.Descriptor.Factory, b.Descriptor.Factory); factory != 0 {
		return factory
	}
	if kind := cmp.Compare(a.Provenance.Kind, b.Provenance.Kind); kind != 0 {
		return kind
	}
	return cmp.Compare(a.Provenance.ID, b.Provenance.ID)
}

func compareDescriptorPhase(a, b Descriptor) int {
	return cmp.Compare(a.requestStage, b.requestStage)
}

func (e *StreamingResponseExecutor) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) (*http.Request, base.ProtocolDisposition, error) {
	if e == nil {
		if next != nil {
			next.ServeHTTP(w, r)
		}
		return r, 0, nil
	}
	request := r
	if len(e.terminals) == 0 {
		if lifecycle := apisixctx.GetRequestLifecycle(request); lifecycle != nil &&
			lifecycle.ResponseSource() == apisixctx.ResponseSourceUnknown {
			apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceUpstream)
		}
		if next != nil {
			next.ServeHTTP(w, request)
		}
		return request, 0, nil
	}
	for _, candidate := range e.terminals {
		result, err := guardStreamingValue(candidate.Identity, PhaseProtocol, func() (streamingProtocolResult, error) {
			disposition, replacement, source, runErr := candidate.Terminal.RunExclusiveProtocol(
				protectStreamingDownstreamWriter(w),
				request,
				guardContinuation(next),
			)
			return streamingProtocolResult{
				disposition: disposition,
				request:     replacement,
				source:      source,
			}, runErr
		})
		disposition, replacement, source := result.disposition, result.request, result.source
		if replacement == nil {
			replacement = request
		}
		request = replacement
		if lifecycle := apisixctx.GetRequestLifecycle(request); lifecycle != nil {
			lifecycle.SetFinalRequest(request)
		}
		if source != apisixctx.ResponseSourceUnknown {
			apisixctx.SetRequestResponseSource(request, source)
		}
		if err != nil {
			return request, disposition, err
		}
		switch disposition {
		case base.ProtocolResponded, base.ProtocolHijacked:
			return request, disposition, nil
		default:
			return request, disposition, fmt.Errorf(
				"exclusive protocol identity=%q returned invalid disposition %d",
				candidate.Identity,
				disposition,
			)
		}
	}
	if next != nil {
		next.ServeHTTP(w, request)
	}
	return request, 0, nil
}

// Then installs streaming wrappers around the normal upstream continuation.
// Exclusive protocol owners run exactly once before that continuation.
func (e *StreamingResponseExecutor) Then(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var committed atomic.Bool
		tracked := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(status int) {
					if status >= http.StatusOK || status == http.StatusSwitchingProtocols {
						committed.Store(true)
					}
					writeHeader(status)
				}
			},
			Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(body []byte) (int, error) {
					committed.Store(true)
					return write(body)
				}
			},
			Flush: func(flush httpsnoop.FlushFunc) httpsnoop.FlushFunc {
				return func() { committed.Store(true); flush() }
			},
			FlushError: func(flush httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
				return func() error { committed.Store(true); return flush() }
			},
		})
		wrapped, request, finish, err := e.wrapStreamingResponse(tracked, r)
		if err != nil {
			result := finish.finish(err)
			if panicErr := streamingPanicError(err); panicErr != nil {
				logStreamingFinishPanics(result.Panics)
				panic(panicErr)
			}
			panicFirstStreamingFinish(result)
			if !committed.Load() {
				writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			panic(http.ErrAbortHandler)
		}
		var panicValue any
		var runErr error
		func() {
			defer func() { panicValue = recover() }()
			_, _, runErr = e.RunExclusiveProtocol(
				wrapped,
				request,
				http.HandlerFunc(func(nextWriter http.ResponseWriter, nextRequest *http.Request) {
					if lifecycle := apisixctx.GetRequestLifecycle(nextRequest); lifecycle != nil &&
						lifecycle.ResponseSource() == apisixctx.ResponseSourceUnknown {
						apisixctx.SetRequestResponseSource(nextRequest, apisixctx.ResponseSourceUpstream)
					}
					if next != nil {
						next.ServeHTTP(nextWriter, nextRequest)
					}
				}),
			)
		}()
		if panicValue != nil {
			result := finish.finish(errStreamingPanic)
			if setupErr, ok := panicValue.(streamingSetupError); ok && !committed.Load() {
				if panicErr := streamingPanicError(setupErr.err); panicErr != nil {
					logStreamingFinishPanics(result.Panics)
					panic(panicErr)
				}
				panicFirstStreamingFinish(result)
				writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			logStreamingFinishPanics(result.Panics)
			panic(panicValue)
		}
		panicErr := streamingPanicError(runErr)
		finishCause := runErr
		if panicErr != nil {
			finishCause = errStreamingPanic
		}
		finishResult := finish.finish(finishCause)
		if panicErr != nil {
			logStreamingFinishPanics(finishResult.Panics)
			panic(panicErr)
		}
		panicFirstStreamingFinish(finishResult)
		if runErr == nil {
			runErr = finishResult.Err
		}
		if runErr != nil {
			if committed.Load() {
				panic(http.ErrAbortHandler)
			}
			writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
		}
	})
}

func copyHeader(dst, src http.Header) {
	clear(dst)
	for field, values := range src {
		dst[field] = append([]string(nil), values...)
	}
}
