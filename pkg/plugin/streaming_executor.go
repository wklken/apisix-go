package plugin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync/atomic"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
)

// StreamingResponseExecutor owns streaming header filters, body wrappers and
// the optional exclusive protocol owner for one generation. It is immutable;
// all per-request state lives in the returned handler/writer chain.
type StreamingResponseExecutor struct {
	bindings  []Binding
	terminals []RouteTerminalCandidate
}

type streamingFinish struct {
	once        atomic.Bool
	closers     []io.Closer
	finalizers  []base.StreamingResponseFinalizer
	compression *streamingCompression
}

type compressionOfferEntry struct {
	plugin CompressionOfferPlugin
	offer  compression.Offer
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

func (f *streamingFinish) finish(cause error) error {
	if f == nil || !f.once.CompareAndSwap(false, true) {
		return nil
	}
	var first error
	for _, closer := range slices.Backward(f.closers) {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, finalizer := range slices.Backward(f.finalizers) {
		if err := finalizer.FinishStreamingResponse(cause); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func NewStreamingResponseExecutor(bindings []Binding) (*StreamingResponseExecutor, error) {
	cloned := cloneBindings(bindings)
	executor := &StreamingResponseExecutor{bindings: cloned}
	for _, binding := range cloned {
		if binding.Plugin == nil {
			return nil, fmt.Errorf(
				"streaming binding has nil plugin (factory=%q resource=%s/%s)",
				binding.factoryName, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
		capability, err := responseCapabilityForBinding(binding)
		if err != nil {
			return nil, err
		}
		if capability.ExclusiveProtocol != ProtocolNone {
			terminal, ok := binding.Plugin.(base.ExclusiveProtocolTerminal)
			if !ok {
				continue // route-owned protocol terminals are supplied separately
			}
			executor.terminals = append(executor.terminals, RouteTerminalCandidate{
				Identity: binding.factoryName, Scope: binding.Scope, Priority: binding.Plugin.GetPriority(),
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
	state := base.StreamingResponseState{Status: http.StatusOK, Header: w.Header().Clone(), Trailer: make(http.Header)}
	if err := e.runHeaderFilters(r, &state); err != nil {
		return nil, r, nil, err
	}
	copyHeader(w.Header(), state.Header)
	request, negotiation, err := e.registerCompressionOffers(r)
	if err != nil {
		return nil, r, nil, err
	}
	finish := &streamingFinish{compression: negotiation}
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
	dynamicHeaders := make([]Binding, 0)
	for _, binding := range effective.all() {
		if binding.Provenance.Kind != ResourceConsumer && binding.Provenance.Kind != ResourceConsumerGroup {
			continue
		}
		capability, err := responseCapabilityForBinding(binding)
		if err != nil {
			return r, err
		}
		buffered, err := MaterializeResponseBindings(EffectiveBindingSet{merged: []Binding{binding}})
		if err != nil {
			return r, err
		}
		if capability.CompressionOffer || capability.BufferedBodyFilter || capability.StreamingBodyFilter ||
			capability.StreamingResponseOwner || capability.ExclusiveProtocol != ProtocolNone || len(buffered) > 0 {
			return r, fmt.Errorf(
				"dynamic response identity=%q resource=%s/%s is not supported",
				binding.factoryName,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
		if capability.HeaderFilter {
			dynamicHeaders = append(dynamicHeaders, binding)
		}
	}
	if len(dynamicHeaders) > 0 {
		return withDynamicStreamingBindings(r, dynamicHeaders), nil
	}
	return r, nil
}

func dynamicHeaderBindingsForEffective(effective EffectiveBindingSet) []Binding {
	bindings := dynamicHeaderBindingsForPartition(nil, effective.global)
	return dynamicHeaderBindingsForPartition(bindings, effective.merged)
}

func dynamicHeaderBindingsForPartition(bindings []Binding, partition []Binding) []Binding {
	for _, binding := range partition {
		if binding.Provenance.Kind != ResourceConsumer && binding.Provenance.Kind != ResourceConsumerGroup {
			continue
		}
		capability, err := responseCapabilityForBinding(binding)
		if err != nil || !capability.HeaderFilter {
			continue
		}
		bindings = append(bindings, binding)
	}
	return bindings
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
				binding.factoryName, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
		if err := plugin.RunStreamingHeaderFilter(r, state); err != nil {
			return fmt.Errorf("factory %q streaming header filter: %w", binding.factoryName, err)
		}
	}
	return nil
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
		if isDualModeResponseBinding(binding, capability) &&
			binding.Plugin.(base.RequestResponseModeSelector).SelectResponseMode(r) !=
				base.RequestResponseModeStreaming {
			continue
		}
		plugin, ok := binding.Plugin.(base.StreamingBodyFilterPlugin)
		if !ok {
			return nil, fmt.Errorf(
				"factory %q declares streaming body filter without callback (resource=%s/%s)",
				binding.factoryName, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
		wrapped, err := plugin.WrapStreamingResponse(current, r)
		if err != nil {
			_ = finish.finish(err)
			return nil, fmt.Errorf("factory %q streaming body wrapper: %w", binding.factoryName, err)
		}
		if wrapped == nil {
			_ = finish.finish(nil)
			return nil, fmt.Errorf("factory %q streaming body wrapper returned nil", binding.factoryName)
		}
		if closer, ok := wrapped.(io.Closer); ok {
			finish.closers = append(finish.closers, closer)
		}
		if finalizer, ok := wrapped.(base.StreamingResponseFinalizer); ok {
			finish.finalizers = append(finish.finalizers, finalizer)
		}
		current = httpsnoop.Wrap(wrapped, httpsnoop.Hooks{})
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
				binding.factoryName, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
		offers := plugin.RegisterCompressionOffers(request, state)
		for _, offer := range offers {
			if offer.Coding == compression.Identity {
				continue
			}
			allOffers = append(allOffers, offer)
			negotiation.offers = append(negotiation.offers, compressionOfferEntry{plugin: plugin, offer: offer})
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
		wrapped, err := entry.plugin.WrapCompression(w, r, f.compression.state, decision)
		if err != nil {
			return nil, err
		}
		if wrapped == nil {
			return nil, fmt.Errorf("compression factory returned nil writer")
		}
		if closer, ok := wrapped.(io.Closer); ok {
			f.closers = append(f.closers, closer)
		}
		if finalizer, ok := wrapped.(base.StreamingResponseFinalizer); ok {
			f.finalizers = append(f.finalizers, finalizer)
		}
		return wrapped, nil
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
		_ = finish.finish(err)
		return err
	}
	wrapped, err := e.wrapBody(inner, request, finish)
	if err != nil {
		_ = finish.finish(err)
		return err
	}
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		commit(wrapped, state)
	}()
	if panicValue != nil {
		_ = finish.finish(fmt.Errorf("streaming commit panic: %v", panicValue))
		panic(panicValue)
	}
	if err := finish.finish(nil); err != nil {
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
		if err != nil || !want(capability) || capability.SeparateSubsystem && binding.factoryName == "mqtt-proxy" {
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
		if binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal || binding.factoryName == "" {
			continue
		}
		indexes[binding.factoryName] = index
	}
	for _, binding := range dynamic {
		if index, ok := indexes[binding.factoryName]; ok {
			merged[index] = binding
			continue
		}
		if binding.factoryName != "" {
			indexes[binding.factoryName] = len(merged)
		}
		merged = append(merged, binding)
	}
	return merged
}

func compareBindings(a, b Binding) int {
	if a.Scope != b.Scope {
		if a.Scope < b.Scope {
			return -1
		}
		return 1
	}
	if a.Stage != b.Stage {
		if a.Stage < b.Stage {
			return -1
		}
		return 1
	}
	if a.Plugin == nil || b.Plugin == nil {
		return 0
	}
	if a.Plugin.GetPriority() > b.Plugin.GetPriority() {
		return -1
	}
	if a.Plugin.GetPriority() < b.Plugin.GetPriority() {
		return 1
	}
	return 0
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
		disposition, replacement, source, err := candidate.Terminal.RunExclusiveProtocol(w, request, next)
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
			if !committed.Load() {
				writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
			}
			return
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
			_ = finish.finish(fmt.Errorf("streaming terminal panic: %v", panicValue))
			if _, ok := panicValue.(streamingSetupError); ok && !committed.Load() {
				writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			panic(panicValue)
		}
		if finishErr := finish.finish(runErr); runErr == nil {
			runErr = finishErr
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
