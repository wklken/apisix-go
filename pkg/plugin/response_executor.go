package plugin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"golang.org/x/net/http/httpguts"
)

type TerminalOwner uint8

const (
	TerminalOwnerOrdinaryProxy TerminalOwner = iota
	TerminalOwnerGlobalNotFound
	TerminalOwnerAIRuntime
	TerminalOwnerKafka
	TerminalOwnerDubbo
	TerminalOwnerHTTPDubbo
)

type TerminalDescriptor struct {
	Owner      TerminalOwner
	Provenance ResourceProvenance
}

type PostResolutionHook func(*http.Request, EffectiveBindingSet) (*http.Request, error)

type BaseCommit func(http.ResponseWriter, *base.ResponseState)

type FinalResponseCommitter interface {
	CommitFinalResponse(http.ResponseWriter, *http.Request, *base.ResponseState, BaseCommit)
}

type directFinalResponseCommitter struct{}

func (directFinalResponseCommitter) CommitFinalResponse(
	w http.ResponseWriter,
	_ *http.Request,
	state *base.ResponseState,
	baseCommit BaseCommit,
) {
	baseCommit(w, state)
}

type BufferedResponseExecutor struct {
	staticBindings []Binding
	staticPlan     []ResponseBinding
	terminal       TerminalDescriptor
	config         base.BufferedResponseConfig
	committer      FinalResponseCommitter
	streaming      *StreamingResponseExecutor
}

func NewBufferedResponseExecutor(
	static []Binding,
	terminal TerminalDescriptor,
	config base.BufferedResponseConfig,
) (*BufferedResponseExecutor, error) {
	if config.MaxBytes <= 0 {
		return nil, fmt.Errorf("buffered response max bytes must be positive: %d", config.MaxBytes)
	}
	cloned, err := resolveBindingsForPlan(static)
	if err != nil {
		return nil, err
	}
	set := bindingsToEffectiveSet(cloned)
	plan, err := materializeResponseBindings(set, hasConditionalTerminalBinding(cloned))
	if err != nil {
		return nil, err
	}
	if err := validateBoundedConflicts(plan, set, terminal); err != nil {
		return nil, err
	}
	return &BufferedResponseExecutor{
		staticBindings: cloned,
		staticPlan:     plan,
		terminal:       terminal,
		config:         config,
		committer:      directFinalResponseCommitter{},
	}, nil
}

func (e *BufferedResponseExecutor) WithFinalResponseCommitter(
	committer FinalResponseCommitter,
) *BufferedResponseExecutor {
	if e == nil {
		return nil
	}
	clone := *e
	if committer == nil {
		clone.committer = directFinalResponseCommitter{}
	} else {
		clone.committer = committer
	}
	clone.staticBindings = cloneBindings(e.staticBindings)
	clone.staticPlan = append([]ResponseBinding(nil), e.staticPlan...)
	return &clone
}

func (e *BufferedResponseExecutor) WithStreamingResponseExecutor(
	streaming *StreamingResponseExecutor,
) *BufferedResponseExecutor {
	if e == nil {
		return nil
	}
	clone := *e
	clone.streaming = streaming
	clone.staticBindings = cloneBindings(e.staticBindings)
	clone.staticPlan = append([]ResponseBinding(nil), e.staticPlan...)
	return &clone
}

func (e *BufferedResponseExecutor) PostResolutionHook(
	r *http.Request,
	effective EffectiveBindingSet,
) (*http.Request, error) {
	if e == nil {
		return r, nil
	}
	execution, ok := r.Context().Value(responseExecutionKey{}).(*responseExecution)
	if !ok || execution == nil {
		return r, errors.New("post-resolution hook missing request-local response execution")
	}
	if execution.hookCalled {
		execution.internalFailure = true
		return r, errors.New("post-resolution hook called more than once")
	}
	execution.hookCalled = true
	execution.request = r
	cloned := effective.clone()
	plan, err := materializeResponseBindings(cloned, hasConditionalTerminalEffective(cloned))
	if err != nil {
		execution.internalFailure = true
		return r, err
	}
	if err := validateBoundedConflicts(plan, effective, e.terminal); err != nil {
		execution.internalFailure = true
		return r, err
	}
	if len(plan) > 0 && execution.mode == responseModeTransparent {
		execution.internalFailure = true
		return r, errors.New("bounded response plan selected after transparent response started")
	}
	execution.plan = append([]ResponseBinding(nil), plan...)
	if len(plan) > 0 {
		execution.mode = responseModeBounded
	} else {
		execution.mode = responseModeTransparent
		execution.replayCapture()
	}
	return r, nil
}

func (s *responseExecution) selectRequestResponseMode(r *http.Request) error {
	if s == nil || len(s.plan) == 0 {
		return nil
	}
	selected := base.RequestResponseMode(0)
	dualCount := 0
	for _, binding := range s.plan {
		if !binding.Descriptor.resolved {
			return fmt.Errorf("factory %q has no resolved descriptor", binding.factoryKey)
		}
		capability := binding.Descriptor.responseCapability
		if !isRequestSelectableResponseBinding(Binding{
			Plugin: binding.Plugin, Descriptor: binding.Descriptor,
			Scope: binding.Scope, Provenance: binding.Provenance,
		}, capability) {
			continue
		}
		selector := binding.Plugin.(base.RequestResponseModeSelector)
		mode, err := guardValue(binding.factoryKey, PhaseBodyFilter, func() (base.RequestResponseMode, error) {
			return selector.SelectResponseMode(r), nil
		})
		if panicErr, ok := err.(*PanicError); ok {
			panic(panicErr)
		}
		if err != nil {
			return err
		}
		if mode != base.RequestResponseModeBounded && mode != base.RequestResponseModeStreaming {
			return fmt.Errorf("factory %q selected unsupported request response mode %d", binding.factoryKey, mode)
		}
		if selected != 0 && selected != mode {
			return fmt.Errorf("dual-mode response bindings selected incompatible request modes")
		}
		selected = mode
		dualCount++
	}
	if dualCount == 0 || selected == base.RequestResponseModeBounded {
		return nil
	}
	if dualCount != len(s.plan) {
		return errors.New("streaming request selected with a non-streaming buffered response binding")
	}
	s.request = r
	s.plan = nil
	s.replayCapture()
	return nil
}

func bindingsToEffectiveSet(bindings []Binding) EffectiveBindingSet {
	set := EffectiveBindingSet{}
	for _, binding := range bindings {
		if binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal {
			set.global = append(set.global, binding)
		} else {
			set.merged = append(set.merged, binding)
		}
	}
	return set
}

func (e *BufferedResponseExecutor) begin(dst http.ResponseWriter, r *http.Request) (*http.Request, *responseExecution) {
	if e == nil {
		return r, nil
	}
	capture := base.GetOrCreateTransformResponseWriter(r)
	for field, values := range dst.Header() {
		capture.Header()[field] = append([]string(nil), values...)
	}
	mode := responseModeUndecided
	if len(e.staticPlan) > 0 {
		mode = responseModeProvisionalBounded
	}
	holder := base.NewCacheHitResponseHolder()
	execution := &responseExecution{
		executor:      e,
		destination:   dst,
		capture:       capture,
		mode:          mode,
		plan:          append([]ResponseBinding(nil), e.staticPlan...),
		request:       r,
		originRequest: r,
		lifecycle:     apisixctx.GetRequestLifecycle(r),
		holder:        holder,
	}
	request := r.WithContext(contextWithResponseExecution(r.Context(), execution))
	request = base.WithCacheHitResponseHolder(request, holder)
	execution.request = request
	execution.writer = httpsnoop.Wrap(dst, execution.hooks())
	return request, execution
}

type responseExecutionKey struct{}

func contextWithResponseExecution(ctx context.Context, execution *responseExecution) context.Context {
	return context.WithValue(ctx, responseExecutionKey{}, execution)
}

type responseMode uint8

const (
	responseModeUndecided responseMode = iota
	responseModeProvisionalBounded
	responseModeTransparent
	responseModeBounded
)

var errBufferedResponseUnsupported = errors.New("buffered response does not support this optional operation")

type responseExecution struct {
	executor             *BufferedResponseExecutor
	destination          http.ResponseWriter
	writer               http.ResponseWriter
	capture              *base.BufferedResponseWriter
	request              *http.Request
	originRequest        *http.Request
	lifecycle            *apisixctx.RequestLifecycle
	holder               *base.CacheHitResponseHolder
	plan                 []ResponseBinding
	mode                 responseMode
	hookCalled           bool
	replayed             bool
	overflow             bool
	unsupported          bool
	internalFailure      bool
	finalResponse        bool
	captureActivity      bool
	transparentCommitted bool
}

func (s *responseExecution) hooks() httpsnoop.Hooks {
	return httpsnoop.Hooks{
		Header: func(_ httpsnoop.HeaderFunc) httpsnoop.HeaderFunc {
			return func() http.Header { return s.header() }
		},
		WriteHeader: func(_ httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) { s.writeHeader(status) }
		},
		Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) { return s.write(body, write) }
		},
		WriteString: func(writeString httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) { return s.writeString(value, writeString) }
		},
		ReadFrom: func(readFrom httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) { return s.readFrom(reader, readFrom) }
		},
		Flush: func(flush httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() { s.flush(flush) }
		},
		FlushError: func(flush httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error { return s.flushError(flush) }
		},
		CloseNotify: func(closeNotify httpsnoop.CloseNotifyFunc) httpsnoop.CloseNotifyFunc {
			return closeNotify
		},
		Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) { return s.hijack(hijack) }
		},
		Push: func(push httpsnoop.PushFunc) httpsnoop.PushFunc {
			return func(target string, opts *http.PushOptions) error { return s.push(target, opts, push) }
		},
		SetReadDeadline: func(set httpsnoop.SetReadDeadlineFunc) httpsnoop.SetReadDeadlineFunc {
			return set
		},
		SetWriteDeadline: func(set httpsnoop.SetWriteDeadlineFunc) httpsnoop.SetWriteDeadlineFunc {
			return set
		},
		EnableFullDuplex: func(set httpsnoop.EnableFullDuplexFunc) httpsnoop.EnableFullDuplexFunc {
			return func() error {
				if s.mode == responseModeBounded || s.mode == responseModeProvisionalBounded {
					s.unsupported = true
					return errBufferedResponseUnsupported
				}
				if s.mode == responseModeUndecided {
					s.replayCapture()
				}
				return set()
			}
		},
	}
}

func (s *responseExecution) header() http.Header {
	if s.mode == responseModeTransparent {
		return s.destination.Header()
	}
	return s.capture.Header()
}

func (s *responseExecution) writeHeader(status int) {
	if s.mode == responseModeTransparent {
		s.transparentCommitted = true
		s.destination.WriteHeader(status)
		return
	}
	if s.overflow {
		return
	}
	if status >= 200 || status == http.StatusSwitchingProtocols {
		s.finalResponse = true
	}
	s.captureActivity = true
	s.capture.WriteHeader(status)
}

func (s *responseExecution) write(body []byte, transparentWrite httpsnoop.WriteFunc) (int, error) {
	if s.mode == responseModeTransparent {
		s.transparentCommitted = true
		return transparentWrite(body)
	}
	if s.overflow {
		return len(body), nil
	}
	s.captureActivity = true
	if !base.ResponseAllowsBody(s.request.Method, s.capture.StatusCode()) {
		s.finalResponse = true
		return s.capture.Write(body)
	}
	if int64(len(s.capture.Body()))+int64(len(body)) > s.executor.config.MaxBytes {
		if s.mode == responseModeUndecided {
			s.replayCapture()
			s.transparentCommitted = true
			return transparentWrite(body)
		}
		s.overflow = true
		return len(body), nil
	}
	s.finalResponse = true
	return s.capture.Write(body)
}

func (s *responseExecution) writeString(
	value string,
	transparentWrite httpsnoop.WriteStringFunc,
) (int, error) {
	if s.mode == responseModeTransparent {
		s.transparentCommitted = true
		return transparentWrite(value)
	}
	return s.write([]byte(value), func(body []byte) (int, error) {
		return transparentWrite(string(body))
	})
}

func (s *responseExecution) readFrom(
	reader io.Reader,
	transparentReadFrom httpsnoop.ReadFromFunc,
) (int64, error) {
	if s.mode == responseModeTransparent {
		s.transparentCommitted = true
		return transparentReadFrom(reader)
	}
	if s.mode == responseModeUndecided {
		s.replayCapture()
		s.transparentCommitted = true
		return transparentReadFrom(reader)
	}
	s.unsupported = true
	return 0, errBufferedResponseUnsupported
}

func (s *responseExecution) flush(transparentFlush httpsnoop.FlushFunc) {
	if s.mode == responseModeTransparent {
		s.transparentCommitted = true
		transparentFlush()
		return
	}
	if s.mode == responseModeUndecided {
		s.replayCapture()
		s.transparentCommitted = true
		transparentFlush()
		return
	}
	// A bounded response deliberately delays downstream visibility until every
	// body transform has completed. Absorb upstream flush hints instead of
	// turning an otherwise valid chunked response into a gateway failure.
}

func (s *responseExecution) flushError(transparentFlush httpsnoop.FlushErrorFunc) error {
	if s.mode == responseModeTransparent {
		s.transparentCommitted = true
		return transparentFlush()
	}
	if s.mode == responseModeUndecided {
		s.replayCapture()
		s.transparentCommitted = true
		return transparentFlush()
	}
	// See flush: completing the bounded transform is the flush boundary.
	return nil
}

func (s *responseExecution) hijack(
	transparentHijack httpsnoop.HijackFunc,
) (net.Conn, *bufio.ReadWriter, error) {
	if s.mode == responseModeUndecided {
		s.replayCapture()
	}
	if s.mode != responseModeTransparent {
		s.unsupported = true
		return nil, nil, errBufferedResponseUnsupported
	}
	s.transparentCommitted = true
	return transparentHijack()
}

func (s *responseExecution) push(
	target string,
	opts *http.PushOptions,
	transparentPush httpsnoop.PushFunc,
) error {
	if s.mode == responseModeUndecided {
		s.replayCapture()
	}
	if s.mode != responseModeTransparent {
		s.unsupported = true
		return errBufferedResponseUnsupported
	}
	s.transparentCommitted = true
	return transparentPush(target, opts)
}

func (s *responseExecution) replayCapture() {
	if s.replayed || s.capture == nil {
		return
	}
	s.replayed = true
	s.mode = responseModeTransparent
	replaceResponseHeader(s.destination.Header(), nil)
	committedFinal := s.capture.CommitCaptured(s.destination)
	s.transparentCommitted = s.transparentCommitted || committedFinal || s.captureActivity
}

func (s *responseExecution) complete() {
	if s.mode == responseModeTransparent ||
		(len(s.plan) == 0 && !s.hookCalled && len(s.executor.staticPlan) == 0) {
		if s.mode != responseModeTransparent {
			s.replayCapture()
		}
		return
	}
	if !s.hookCalled {
		s.plan = append([]ResponseBinding(nil), s.executor.staticPlan...)
	}
	if len(s.plan) == 0 {
		s.replayCapture()
		return
	}
	lifecycle := s.lifecycle
	if lifecycle == nil {
		panic(http.ErrAbortHandler)
	}
	finalRequest := lifecycle.FinalRequest()
	if finalRequest == nil || finalRequest.Context().Err() != nil {
		panic(http.ErrAbortHandler)
	}
	s.request = finalRequest
	if apisixctx.GetRequestLifecycle(finalRequest) != lifecycle ||
		base.CacheHitResponseHolderFromRequest(finalRequest) != s.holder {
		s.fail(http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if s.internalFailure {
		s.fail(http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if s.overflow || s.unsupported {
		s.fail(http.StatusBadGateway, "Bad Gateway")
		return
	}
	source := lifecycle.ResponseSource()
	if source == apisixctx.ResponseSourceUnknown {
		s.fail(http.StatusInternalServerError, "Internal Server Error")
		return
	}

	holder := s.holder
	if source == apisixctx.ResponseSourceCacheHit {
		cached, published, err := holder.ConsumePublished()
		if err != nil || !published {
			s.fail(http.StatusInternalServerError, "Internal Server Error")
			return
		}
		state := base.ResponseState(cached)
		if invalidFinalState(state, s.executor.config.MaxBytes) {
			s.fail(http.StatusBadGateway, "Bad Gateway")
			return
		}
		s.commitState(state)
		return
	}
	if _, published, err := holder.ConsumePublished(); err != nil || published {
		s.fail(http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if !s.finalResponse {
		s.fail(http.StatusInternalServerError, "Internal Server Error")
		return
	}
	header := s.capture.Header().Clone()
	state := base.ResponseState{
		Status: s.capture.StatusCode(),
		Header: header,
		Body:   slices.Clone(s.capture.Body()),
	}
	state.Trailer = base.ExtractResponseTrailers(state.Header)
	if invalidFinalState(state, s.executor.config.MaxBytes) {
		s.fail(http.StatusBadGateway, "Bad Gateway")
		return
	}
	if err := s.runTransforms(&state, source); err != nil {
		apisixctx.SetRequestResponseSource(s.request, apisixctx.ResponseSourceAPISIX)
		s.fail(http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if invalidFinalState(state, s.executor.config.MaxBytes) {
		s.fail(http.StatusBadGateway, "Bad Gateway")
		return
	}
	s.runStores(state, source)
	s.commitState(state)
}

func (s *responseExecution) runTransforms(
	state *base.ResponseState,
	source apisixctx.ResponseSource,
) error {
	for _, phase := range []ResponsePhaseMask{ResponsePhaseHeader, ResponsePhaseBufferedBody} {
		for _, binding := range s.plan {
			if binding.Phases&phase == 0 {
				continue
			}
			switch phase {
			case ResponsePhaseHeader:
				if !eligible(binding, source, PhaseHeaderFilter) {
					continue
				}
				err := guardCall(binding.factoryKey, PhaseHeaderFilter, func() error {
					return binding.Plugin.(base.HeaderFilterPlugin).RunHeaderFilter(s.request, state)
				})
				if panicErr, ok := err.(*PanicError); ok {
					panic(panicErr)
				}
				if err != nil {
					return err
				}
			case ResponsePhaseBufferedBody:
				if !eligible(binding, source, PhaseBodyFilter) {
					continue
				}
				err := guardCall(binding.factoryKey, PhaseBodyFilter, func() error {
					return binding.Plugin.(base.BufferedBodyFilterPlugin).RunBufferedBodyFilter(
						s.request,
						state,
					)
				})
				if panicErr, ok := err.(*PanicError); ok {
					panic(panicErr)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *responseExecution) runStores(
	state base.ResponseState,
	source apisixctx.ResponseSource,
) {
	for _, binding := range s.plan {
		if binding.Phases&ResponsePhaseFinalStore == 0 || !eligible(binding, source, PhaseBodyFilter) {
			continue
		}
		err := guardCall(binding.factoryKey, PhaseBodyFilter, func() error {
			return binding.Plugin.(base.FinalResponseStorePlugin).RunFinalResponseStore(
				s.request,
				base.CloneResponseState(state),
			)
		})
		if panicErr, ok := err.(*PanicError); ok {
			panic(panicErr)
		}
		if err != nil {
			logger.Errorf(
				"final response store failed factory=%q resource=%s/%q: %v",
				sanitizeDiagnostic(binding.factoryKey),
				binding.Provenance.Kind,
				sanitizeDiagnostic(binding.Provenance.ID),
				sanitizeDiagnostic(err.Error()),
			)
		}
	}
}

func eligible(binding ResponseBinding, source apisixctx.ResponseSource, phase Phase) bool {
	if source == apisixctx.ResponseSourceCacheHit {
		return false
	}
	if checker, ok := binding.Plugin.(base.ResponseEligibility); ok {
		eligible, err := guardValue(binding.factoryKey, phase, func() (bool, error) {
			return checker.AppliesToResponseSource(source), nil
		})
		if panicErr, ok := err.(*PanicError); ok {
			panic(panicErr)
		}
		return eligible
	}
	return source == apisixctx.ResponseSourceUpstream
}

func (s *responseExecution) commitState(state base.ResponseState) {
	var called atomic.Bool
	baseCommit := func(dst http.ResponseWriter, final *base.ResponseState) {
		if !called.CompareAndSwap(false, true) {
			panic("buffered response baseCommit called more than once")
		}
		s.capture.CommitFinalResponse(dst, *final)
	}
	committer := s.executor.committer
	if committer == nil {
		committer = directFinalResponseCommitter{}
	}
	commit := baseCommit
	var streamingErr error
	if s.executor.streaming != nil {
		commit = func(dst http.ResponseWriter, final *base.ResponseState) {
			if streamingErr != nil {
				return
			}
			streamingErr = s.executor.streaming.CommitResponse(dst, s.request, final, baseCommit)
		}
	}
	committer.CommitFinalResponse(s.destination, s.request, &state, commit)
	if streamingErr != nil {
		if called.Load() {
			panic(http.ErrAbortHandler)
		}
		s.fail(http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if !called.Load() {
		panic("buffered response committer did not call baseCommit")
	}
}

func (s *responseExecution) fail(status int, text string) {
	if s.lifecycle != nil {
		s.lifecycle.SetResponseSource(apisixctx.ResponseSourceAPISIX)
	}
	apisixctx.SetRequestResponseSource(s.originRequest, apisixctx.ResponseSourceAPISIX)
	apisixctx.SetRequestResponseSource(s.request, apisixctx.ResponseSourceAPISIX)
	state := base.ResponseState{
		Status: status,
		Header: http.Header{"Content-Type": {"application/json; charset=UTF-8"}},
		Body:   fmt.Appendf(nil, `{"message":"%s"}`, text),
	}
	s.capture.CommitFinalResponse(s.destination, state)
}

func invalidFinalState(state base.ResponseState, maxBytes int64) bool {
	return state.Status < 200 || state.Status > 999 ||
		hasTrailer(state.Header) ||
		invalidTrailer(state.Trailer) ||
		int64(len(state.Body)) > maxBytes
}

func invalidTrailer(trailer http.Header) bool {
	for field, values := range trailer {
		if !httpguts.ValidHeaderFieldName(field) || !httpguts.ValidTrailerHeader(field) {
			return true
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return true
			}
		}
	}
	return false
}

func replaceResponseHeader(dst, src http.Header) {
	clear(dst)
	for field, values := range src {
		dst[field] = append([]string(nil), values...)
	}
}

func sanitizeDiagnostic(value string) string {
	const maxLength = 128
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, value)
	if len(value) > maxLength {
		return value[:maxLength]
	}
	return value
}

func hasTrailer(header http.Header) bool {
	for field := range header {
		if strings.EqualFold(field, "Trailer") || strings.HasPrefix(strings.ToLower(field), "trailer:") {
			return true
		}
	}
	return false
}

func validateBoundedConflicts(
	plan []ResponseBinding,
	effective EffectiveBindingSet,
	terminal TerminalDescriptor,
) error {
	if len(plan) == 0 {
		return nil
	}
	if terminal.Owner != TerminalOwnerOrdinaryProxy && terminal.Owner != TerminalOwnerGlobalNotFound {
		return fmt.Errorf(
			"bounded response identity=%q resource=%s/%s conflicts with terminal owner=%d resource=%s/%s",
			plan[0].factoryKey,
			plan[0].Provenance.Kind,
			plan[0].Provenance.ID,
			terminal.Owner,
			terminal.Provenance.Kind,
			terminal.Provenance.ID,
		)
	}
	allBufferedDualMode := responseBindingsAreDualMode(plan)
	for _, binding := range append(append([]Binding(nil), effective.global...), effective.merged...) {
		capability, err := responseCapabilityForBinding(binding)
		if err != nil {
			return err
		}
		if allBufferedDualMode && isDualModeResponseBinding(binding, capability) {
			continue
		}
		if allBufferedDualMode && capability.ExclusiveProtocol == ProtocolAI {
			continue
		}
		conflict := capability.StreamingResponseOwner || capability.ExclusiveProtocol != ProtocolNone ||
			capability.StreamingBodyFilter && !compatibleBoundedAdapter(binding, capability)
		if conflict {
			return fmt.Errorf(
				"bounded response identity=%q resource=%s/%s conflicts with %q resource=%s/%s",
				plan[0].factoryKey,
				plan[0].Provenance.Kind,
				plan[0].Provenance.ID,
				binding.Descriptor.Factory,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
		if (binding.Descriptor.Factory == "serverless-pre-function" || binding.Descriptor.Factory == "serverless-post-function") &&
			binding.Descriptor.requestStage == RequestStageLegacy {
			return fmt.Errorf(
				"bounded response identity=%q resource=%s/%s conflicts with %q log phase resource=%s/%s",
				plan[0].factoryKey,
				plan[0].Provenance.Kind,
				plan[0].Provenance.ID,
				binding.Descriptor.Factory,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
	}
	return nil
}
