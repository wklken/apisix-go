package plugin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// Scope identifies the materialized source scope of a plugin binding.
type Scope uint8

const (
	ScopeSystem Scope = iota
	ScopeGlobal
	ScopeRoute
	ScopeConsumer
)

// RequestStage identifies the request-stage owner for a plugin binding.
type RequestStage uint8

const (
	RequestStageNone RequestStage = iota
	RequestStageRewrite
	RequestStageConsumerRewrite
	RequestStageAccess
	RequestStageBeforeProxy
)

// ResourceKind identifies the source resource that materialized a binding.
type ResourceKind string

const (
	ResourceSystem        ResourceKind = "system"
	ResourceGlobalRule    ResourceKind = "global_rule"
	ResourceRoute         ResourceKind = "route"
	ResourceService       ResourceKind = "service"
	ResourcePluginConfig  ResourceKind = "plugin_config"
	ResourceConsumer      ResourceKind = "consumer"
	ResourceConsumerGroup ResourceKind = "consumer_group"
)

// ResourceProvenance is kept by value so diagnostics cannot lose the source
// identity while route/plugin configuration maps are merged.
type ResourceProvenance struct {
	Kind ResourceKind
	ID   string
}

// Binding is the immutable executor input for one materialized plugin.
type Binding struct {
	Plugin       Plugin
	Descriptor   Descriptor
	Priority     int
	Scope        Scope
	Provenance   ResourceProvenance
	InstanceKey  InstanceKey
	logPolicy    base.LogCapturePolicy
	logPolicySet bool
}

type ConsumerIdentity struct {
	Username   string
	GroupID    string
	AuthSource string
}

type ConsumerCacheKey struct {
	ConsumerID     string
	ConsumerDigest [32]byte
	GroupID        string
	GroupDigest    [32]byte
	RouteID        string
	ServiceID      string
}

type ConsumerResolution struct {
	Bindings []Binding
	Request  *http.Request
	CacheKey ConsumerCacheKey
	Identity ConsumerIdentity
	Resolved bool
}

type ConsumerBindingResolver func(*http.Request) (ConsumerResolution, error)

// EffectiveBindingSet keeps the two Plan 14 partitions separate so response
// materialization can execute global/system winners before route/consumer
// winners without reconstructing them after the terminal returns.
type EffectiveBindingSet struct {
	global []Binding
	merged []Binding
}

func (s EffectiveBindingSet) all() []Binding {
	bindings := make([]Binding, 0, len(s.global)+len(s.merged))
	bindings = append(bindings, s.global...)
	return append(bindings, s.merged...)
}

func (s EffectiveBindingSet) clone() EffectiveBindingSet {
	return EffectiveBindingSet{
		global: cloneBindings(s.global),
		merged: cloneBindings(s.merged),
	}
}

func cloneEffectiveBindingSet(set EffectiveBindingSet) EffectiveBindingSet {
	return set.clone()
}

// RequestPipeline owns the explicit Plan 14 request order. Static bindings
// are cloned at construction; resolution data is merged per request so no
// mutable request, auth, or override state can leak between requests.
type RequestPipeline struct {
	bindings          []Binding
	resolve           ConsumerBindingResolver
	responseExecutor  *BufferedResponseExecutor
	streamingExecutor *StreamingResponseExecutor
	logExecutor       *LogExecutor
	deferBeforeProxy  bool
}

type preparedStaticPipeline struct {
	effective EffectiveBindingSet
	handler   http.Handler
}

type staticCORSAuthenticationState struct {
	destination          http.ResponseWriter
	beforeAuthentication http.Header
}

type staticCORSAuthenticationStateKey struct{}

type requestPhaseWithExecutorState struct {
	base.RequestPhasePlugin
}

func (p requestPhaseWithExecutorState) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	result := p.RequestPhasePlugin.RunRequestPhase(w, r)
	state := r.Context().Value(staticCORSAuthenticationStateKey{})
	if state != nil && result.Request != nil &&
		result.Request.Context().Value(staticCORSAuthenticationStateKey{}) == nil {
		*result.Request = *result.Request.WithContext(context.WithValue(
			result.Request.Context(),
			staticCORSAuthenticationStateKey{},
			state,
		))
	}
	return result
}

func NewRequestPipeline(bindings []Binding, resolve ConsumerBindingResolver) RequestPipeline {
	return RequestPipeline{bindings: cloneBindings(bindings), resolve: resolve}
}

func (p RequestPipeline) Then(terminal http.Handler) http.Handler {
	return p.ThenWithPostResolutionHook(terminal, nil)
}

// WithBufferedResponseExecutor returns a value copy that owns one immutable
// response executor reference. Request-local capture state is created by Then,
// never stored on the pipeline itself.
func (p RequestPipeline) WithBufferedResponseExecutor(
	executor *BufferedResponseExecutor,
) RequestPipeline {
	p.responseExecutor = executor
	return p
}

func (p RequestPipeline) WithStreamingResponseExecutor(
	executor *StreamingResponseExecutor,
) RequestPipeline {
	p.streamingExecutor = executor
	return p
}

func (p RequestPipeline) WithLogExecutor(executor *LogExecutor) RequestPipeline {
	p.logExecutor = executor
	return p
}

// WithBeforeProxyHooksAtTransport defers registered before-proxy hooks to a
// transport installed with WrapBeforeProxyHooks. The ordinary prepared HTTP
// proxy uses this seam so ReverseProxy's Director can materialize the selected
// upstream and pass_host before the hook observes the request.
func (p RequestPipeline) WithBeforeProxyHooksAtTransport() RequestPipeline {
	p.deferBeforeProxy = true
	return p
}

// ThenWithPostResolutionHook inserts one hook after consumer/group winners are
// merged and before any later request stage or terminal runs.
func (p RequestPipeline) ThenWithPostResolutionHook(
	terminal http.Handler,
	hook PostResolutionHook,
) http.Handler {
	if p.streamingExecutor != nil {
		hook = chainPostResolutionHooks(hook, p.streamingExecutor.PostResolutionHook)
	}
	if p.responseExecutor != nil {
		hook = chainPostResolutionHooks(hook, p.responseExecutor.PostResolutionHook)
	}
	if terminal == nil {
		terminal = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	}
	staticEffective := mergeEffectiveBindingSet(p.bindings, nil)
	preparedStatic := preparedStaticPipeline{
		effective: staticEffective,
		handler:   p.buildPostResolutionHandler(staticEffective, terminal, nil),
	}
	plainAfterAuthentication := p.buildPlainResolvedHandler(terminal, hook, &preparedStatic)
	plainHandler := p.wrapAuthentication(plainAfterAuthentication)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := r
		if p.logExecutor != nil {
			var err error
			request, err = p.logExecutor.Prepare(request)
			if err != nil {
				registered := p.logExecutor.RegisterComposite(request)
				recordLogPreparationFailure(err, registered)
				p.writeLogPreparationFailure(w, request, err, nil)
				return
			}
			defer func() {
				recovered := recover()
				if recovered != nil {
					latest := finalLifecycleRequest(request)
					sealErr := p.logExecutor.SealFinalRequest(latest)
					registered := p.logExecutor.RegisterComposite(latest)
					recordLogPreparationFailure(sealErr, registered)
					panic(recovered)
				}
			}()
		}
		var execution *responseExecution
		if p.responseExecutor != nil {
			request, execution = p.responseExecutor.begin(w, request)
		}
		if execution == nil {
			plainHandler.ServeHTTP(w, request)
			p.sealAndRegisterAfterRequest(w, request, nil)
			return
		}
		// Authentication plugins may replace the request without inheriting
		// its context. Carry the response execution explicitly to the resolver
		// boundary so post-commit fallback decisions never depend on that
		// replaceable context.
		afterAuthentication := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.runResolved(w, r, terminal, hook, execution)
		})
		p.wrapAuthentication(afterAuthentication).ServeHTTP(execution.writer, request)
		execution.complete()
		p.sealAndRegisterAfterRequest(w, request, execution)
	})
}

func chainPostResolutionHooks(first, second PostResolutionHook) PostResolutionHook {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return func(r *http.Request, effective EffectiveBindingSet) (*http.Request, error) {
		replacement, err := first(r, effective)
		if err != nil {
			return r, err
		}
		if replacement == nil {
			replacement = r
		}
		return second(replacement, effective)
	}
}

func (p RequestPipeline) wrapAuthentication(next http.Handler) http.Handler {
	bindings := make([]Binding, 0)
	corsBindings := make([]Binding, 0)
	preAuthentication := make([]Binding, 0)
	globalRewrite := make([]Binding, 0)
	systemRewrite := make([]Binding, 0)
	for _, binding := range p.bindings {
		if binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal || binding.Scope == ScopeRoute {
			if binding.Descriptor.Factory == "cors" {
				if binding.Plugin != nil {
					corsBindings = append(corsBindings, binding)
				}
				continue
			}
			if binding.Descriptor.authenticatesConsumer {
				bindings = append(bindings, binding)
				continue
			}
			if binding.Descriptor.preAuthentication {
				preAuthentication = append(preAuthentication, binding)
				continue
			}
			if binding.Descriptor.requestStage == RequestStageRewrite {
				switch binding.Scope {
				case ScopeGlobal:
					globalRewrite = append(globalRewrite, binding)
				case ScopeSystem:
					systemRewrite = append(systemRewrite, binding)
				}
			}
		}
	}
	next = wrapAuthenticationWithStaticCORS(next, bindings, corsBindings)
	next = wrapRequestStageBindings(next, preAuthentication)
	next = wrapRequestStageBindings(next, globalRewrite)
	next = wrapRequestStageBindings(next, systemRewrite)
	return next
}

func isStaticPreAuthenticationBinding(binding Binding) bool {
	if binding.Plugin == nil || !binding.Descriptor.preAuthentication {
		return false
	}
	return binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal || binding.Scope == ScopeRoute
}

func (p RequestPipeline) postAuthenticationBindings(bindings []Binding) []Binding {
	preAuthenticationFactories := make(map[string]struct{})
	for _, binding := range p.bindings {
		if isStaticPreAuthenticationBinding(binding) {
			preAuthenticationFactories[binding.Descriptor.Factory] = struct{}{}
		}
	}
	return slices.DeleteFunc(bindings, func(binding Binding) bool {
		if !binding.Descriptor.preAuthentication {
			return false
		}
		_, alreadyRan := preAuthenticationFactories[binding.Descriptor.Factory]
		return alreadyRan
	})
}

// wrapAuthenticationWithStaticCORS keeps static CORS in front of
// authentication so it can answer preflight requests and decorate early
// authentication responses. After authentication succeeds, the provisional
// headers are discarded and the post-resolution CORS winner owns the response.
func wrapAuthenticationWithStaticCORS(
	next http.Handler,
	authBindings []Binding,
	corsBindings []Binding,
) http.Handler {
	if len(corsBindings) == 0 {
		return wrapRequestStageBindings(next, authBindings)
	}
	ordered := append([]Binding(nil), corsBindings...)
	slices.SortStableFunc(ordered, compareBindings)
	afterAuthentication := http.HandlerFunc(func(provisional http.ResponseWriter, r *http.Request) {
		state, ok := r.Context().Value(staticCORSAuthenticationStateKey{}).(*staticCORSAuthenticationState)
		if !ok || state == nil {
			panic("static CORS authentication state is missing")
		}
		// Authentication succeeded. Preserve only the header changes made
		// by authentication; provisional static CORS headers are discarded
		// so the post-resolution winner owns the response.
		applyHeaderDelta(
			state.destination.Header(),
			state.beforeAuthentication,
			provisional.Header(),
		)
		*r = *r.WithContext(context.WithValue(r.Context(), staticCORSAuthenticationStateKey{}, nil))
		next.ServeHTTP(state.destination, r)
	})
	authentication := wrapRequestStageBindings(afterAuthentication, authBindings)
	handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, ok := r.Context().Value(staticCORSAuthenticationStateKey{}).(*staticCORSAuthenticationState)
		if !ok || state == nil {
			panic("static CORS authentication state is missing")
		}
		state.beforeAuthentication = w.Header().Clone()
		authentication.ServeHTTP(w, r)
	}))
	for _, binding := range ordered {
		if binding.Plugin != nil {
			handler = pluginMiddlewareHandler(binding, handler)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &staticCORSAuthenticationState{destination: w}
		request := r.WithContext(context.WithValue(r.Context(), staticCORSAuthenticationStateKey{}, state))
		handler.ServeHTTP(provisionalResponseWriter(w), request)
	})
}

func applyHeaderDelta(dst, before, after http.Header) {
	for field, values := range after {
		if slices.Equal(values, before.Values(field)) {
			continue
		}
		dst[field] = append([]string(nil), values...)
	}
	for field := range before {
		if _, ok := after[field]; !ok {
			dst.Del(field)
		}
	}
}

func provisionalResponseWriter(dst http.ResponseWriter) http.ResponseWriter {
	header := dst.Header().Clone()
	committed := false
	commit := func() {
		if committed {
			return
		}
		committed = true
		replaceResponseHeader(dst.Header(), header)
	}
	return httpsnoop.Wrap(dst, httpsnoop.Hooks{
		Header: func(httpsnoop.HeaderFunc) httpsnoop.HeaderFunc {
			return func() http.Header { return header }
		},
		WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				commit()
				writeHeader(status)
			}
		},
		Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				commit()
				return write(body)
			}
		},
		WriteString: func(writeString httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) {
				commit()
				return writeString(value)
			}
		},
		ReadFrom: func(readFrom httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) {
				commit()
				return readFrom(reader)
			}
		},
	})
}

func (p RequestPipeline) runResolved(
	w http.ResponseWriter,
	r *http.Request,
	terminal http.Handler,
	hook PostResolutionHook,
	execution *responseExecution,
) {
	resolution, request, err := p.resolveRequest(r)
	if err != nil {
		p.writeResolutionFailure(w, r, execution, err)
		return
	}
	p.runMaterializedResolved(w, r, request, resolution.Bindings, terminal, hook, execution)
}

func (p RequestPipeline) buildPlainResolvedHandler(
	terminal http.Handler,
	hook PostResolutionHook,
	prepared *preparedStaticPipeline,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolution, request, err := p.resolveRequest(r)
		if err != nil {
			p.writeResolutionFailure(w, r, nil, err)
			return
		}
		if !resolution.Resolved && len(resolution.Bindings) == 0 {
			p.runPreparedStatic(w, r, request, hook, prepared)
			return
		}
		p.runMaterializedResolved(w, r, request, resolution.Bindings, terminal, hook, nil)
	})
}

func (p RequestPipeline) resolveRequest(r *http.Request) (ConsumerResolution, *http.Request, error) {
	resolution := ConsumerResolution{Request: r}
	var err error
	if p.resolve != nil {
		resolution, err = p.resolve(r)
	}
	if err != nil {
		return ConsumerResolution{}, r, err
	}
	request := resolution.Request
	if request == nil {
		request = r
	}
	return resolution, request, nil
}

func (p RequestPipeline) writeResolutionFailure(
	w http.ResponseWriter,
	r *http.Request,
	execution *responseExecution,
	err error,
) {
	logger.Errorf("consumer binding resolution failed: %v", err)
	markResponseInternalFailure(r)
	if execution != nil {
		execution.internalFailure = true
	}
	if responseFailureRequiresAbort(r) || execution != nil && execution.transparentCommitted {
		panic(http.ErrAbortHandler)
	}
	if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
		lifecycle.SetFinalRequest(r)
	}
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
	writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
}

func (p RequestPipeline) runMaterializedResolved(
	w http.ResponseWriter,
	original *http.Request,
	request *http.Request,
	resolved []Binding,
	terminal http.Handler,
	hook PostResolutionHook,
	execution *responseExecution,
) {
	effective := mergeEffectiveBindingSet(p.bindings, resolved)
	if p.logExecutor != nil {
		materializedLogExecutor, logErr := NewLogExecutorFromBindings(effective.all())
		if logErr != nil {
			p.writeLogPreparationFailure(w, request, logErr, execution)
			return
		}
		request, logErr = materializedLogExecutor.Prepare(request)
		if logErr != nil {
			p.writeLogPreparationFailure(w, request, logErr, execution)
			return
		}
	}
	if hook != nil {
		replacement, hookErr := hook(request, effective)
		if hookErr != nil {
			markResponseInternalFailure(original)
			markResponseInternalFailure(request)
			if execution != nil {
				execution.internalFailure = true
			}
			if responseFailureRequiresAbort(original) || responseFailureRequiresAbort(request) ||
				execution != nil && execution.transparentCommitted {
				panic(http.ErrAbortHandler)
			}
			if lifecycle := apisixctx.GetRequestLifecycle(request); lifecycle != nil {
				lifecycle.SetFinalRequest(request)
			}
			apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceAPISIX)
			writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		if replacement != nil {
			request = replacement
			if lifecycle := apisixctx.GetRequestLifecycle(request); lifecycle != nil {
				lifecycle.SetFinalRequest(request)
			}
		}
	}
	inner := p.buildPostResolutionHandler(effective, terminal, execution)
	inner.ServeHTTP(w, request)
}

func (p RequestPipeline) runPreparedStatic(
	w http.ResponseWriter,
	original *http.Request,
	request *http.Request,
	hook PostResolutionHook,
	prepared *preparedStaticPipeline,
) {
	if hook != nil {
		replacement, err := hook(request, prepared.effective)
		if err != nil {
			markResponseInternalFailure(original)
			markResponseInternalFailure(request)
			if responseFailureRequiresAbort(original) || responseFailureRequiresAbort(request) {
				panic(http.ErrAbortHandler)
			}
			if lifecycle := apisixctx.GetRequestLifecycle(request); lifecycle != nil {
				lifecycle.SetFinalRequest(request)
			}
			apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceAPISIX)
			writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		if replacement != nil {
			request = replacement
			if lifecycle := apisixctx.GetRequestLifecycle(request); lifecycle != nil {
				lifecycle.SetFinalRequest(request)
			}
		}
	}
	prepared.handler.ServeHTTP(w, request)
}

func (p RequestPipeline) buildPostResolutionHandler(
	effective EffectiveBindingSet,
	terminal http.Handler,
	execution *responseExecution,
) http.Handler {
	bindings := p.postAuthenticationBindings(effective.all())
	boundary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.streamingExecutor != nil {
			// A post-resolution request stage may replace the request without
			// preserving context values installed by PostResolutionHook.
			if dynamic := dynamicHeaderBindingsForEffective(effective); len(dynamic) > 0 {
				r = withDynamicStreamingBindings(r, dynamic)
			}
		}
		if p.logExecutor != nil {
			if err := p.logExecutor.SealAndRegister(r); err != nil {
				p.writeLogPreparationFailure(w, r, err, execution)
				return
			}
		}
		apisixctx.FinalizeProxyRewrite(r)
		var beforeProxyErr error
		if !p.deferBeforeProxy {
			beforeProxyErr = runBeforeProxyHooks(r)
		}
		if beforeProxyErr != nil {
			status := http.StatusInternalServerError
			message := "Internal Server Error"
			if base.IsBodyTooLarge(beforeProxyErr) {
				status = http.StatusRequestEntityTooLarge
				message = "Request Entity Too Large"
			}
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
			writeStableResponseError(w, status, message)
			return
		}
		if execution != nil {
			if err := execution.selectRequestResponseMode(r); err != nil {
				execution.internalFailure = true
				markResponseInternalFailure(r)
				return
			}
		}
		if p.streamingExecutor != nil {
			if p.responseExecutor == nil {
				p.streamingExecutor.Then(terminalHandler(terminal)).ServeHTTP(w, r)
				return
			}
			if execution != nil && execution.mode == responseModeTransparent {
				p.streamingExecutor.Then(terminalHandler(terminal)).ServeHTTP(w, r)
				return
			}
			_, _, err := p.streamingExecutor.RunExclusiveProtocol(w, r, terminalHandler(terminal))
			if err != nil {
				markResponseInternalFailure(r)
				if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
					lifecycle.SetResponseSource(apisixctx.ResponseSourceAPISIX)
				}
			}
			return
		}
		terminalHandler(terminal).ServeHTTP(w, r)
	})
	handler := http.Handler(boundary)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageBeforeProxy, ScopeRoute)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageBeforeProxy, ScopeConsumer)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageBeforeProxy, ScopeGlobal)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageAccess, ScopeRoute)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageAccess, ScopeConsumer)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageAccess, ScopeGlobal)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageAccess, ScopeSystem)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageConsumerRewrite, ScopeConsumer)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageConsumerRewrite, ScopeRoute)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageConsumerRewrite, ScopeGlobal)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageRewrite, ScopeConsumer)
	handler = wrapScopedRequestStage(handler, bindings, RequestStageRewrite, ScopeRoute)
	return handler
}

func finalLifecycleRequest(fallback *http.Request) *http.Request {
	if lifecycle := apisixctx.GetRequestLifecycle(fallback); lifecycle != nil {
		if request := lifecycle.FinalRequest(); request != nil {
			return request
		}
	}
	return fallback
}

func (p RequestPipeline) sealAndRegisterAfterRequest(
	w http.ResponseWriter,
	request *http.Request,
	execution *responseExecution,
) {
	if p.logExecutor == nil {
		return
	}
	latest := finalLifecycleRequest(request)
	if err := p.logExecutor.SealAndRegister(latest); err != nil {
		p.writeLogPreparationFailure(w, latest, err, execution)
	}
}

func (p RequestPipeline) writeLogPreparationFailure(
	w http.ResponseWriter,
	request *http.Request,
	err error,
	execution *responseExecution,
) {
	markResponseInternalFailure(request)
	if execution != nil {
		execution.internalFailure = true
	}
	if responseFailureRequiresAbort(request) || execution != nil && execution.transparentCommitted {
		panic(http.ErrAbortHandler)
	}
	apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceAPISIX)
	writeStableLogPreparationError(w, err)
}

func wrapRequestStageBindings(next http.Handler, bindings []Binding) http.Handler {
	if len(bindings) == 0 {
		return next
	}
	ordered := append([]Binding(nil), bindings...)
	slices.SortStableFunc(ordered, compareBindings)
	for _, binding := range slices.Backward(ordered) {
		if binding.Plugin == nil {
			continue
		}
		next = requestStageHandler(binding, next)
	}
	return next
}

func wrapScopedRequestStage(next http.Handler, bindings []Binding, stage RequestStage, scope Scope) http.Handler {
	selected := make([]Binding, 0)
	for _, binding := range bindings {
		if binding.Scope != scope || binding.Descriptor.requestStage != stage || binding.Plugin == nil {
			continue
		}
		if binding.Descriptor.authenticatesConsumer {
			continue
		}
		selected = append(selected, binding)
	}
	return wrapRequestStageBindings(next, selected)
}

func requestStageHandler(binding Binding, next http.Handler) http.Handler {
	phase := phaseForRequestStage(binding.Descriptor.requestStage)
	if requestPhase, ok := binding.Plugin.(base.RequestPhasePlugin); ok {
		handler := guardMiddleware(bindingFactory(binding), phase, func(guardedNext http.Handler) http.Handler {
			return base.AdaptRequestPhase(
				requestPhaseWithExecutorState{RequestPhasePlugin: requestPhase},
				guardedNext,
			)
		}, next)
		if handler != nil {
			return handler
		}
		return internalServerErrorHandler()
	}
	return pluginMiddlewareHandler(binding, next)
}

func pluginMiddlewareHandler(binding Binding, next http.Handler) http.Handler {
	handler := guardMiddleware(
		bindingFactory(binding),
		phaseForRequestStage(binding.Descriptor.requestStage),
		binding.Plugin.Handler,
		next,
	)
	if handler != nil {
		return handler
	}
	return internalServerErrorHandler()
}

func bindingFactory(binding Binding) string {
	if binding.Descriptor.Factory != "" {
		return binding.Descriptor.Factory
	}
	if binding.Plugin != nil {
		return binding.Plugin.GetName()
	}
	return ""
}

func internalServerErrorHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	})
}

func invokeBeforeProxyHook(
	r *http.Request,
	registration apisixctx.BeforeProxyHookRegistration,
) error {
	if registration.Owner == "" {
		return registration.Hook(r)
	}
	if strings.TrimSpace(registration.Owner) != registration.Owner {
		return fmt.Errorf("before-proxy hook has invalid owner %q", registration.Owner)
	}
	if _, ok := pluginRegistry[registration.Owner]; !ok {
		return fmt.Errorf("before-proxy hook has unknown owner %q", registration.Owner)
	}
	phase := Phase(registration.Phase)
	if !isRuntimePluginPhase(phase) {
		return fmt.Errorf(
			"before-proxy hook owner %q has invalid phase %q",
			registration.Owner,
			registration.Phase,
		)
	}
	err := guardCall(registration.Owner, phase, func() error { return registration.Hook(r) })
	if panicErr, ok := err.(*PanicError); ok {
		panic(panicErr)
	}
	return err
}

func runBeforeProxyHooks(r *http.Request) error {
	return apisixctx.RunBeforeProxyHookRegistrations(
		r,
		func(registration apisixctx.BeforeProxyHookRegistration) error {
			return invokeBeforeProxyHook(r, registration)
		},
	)
}

type beforeProxyHookRoundTripper struct {
	next http.RoundTripper
}

// WrapBeforeProxyHooks runs registered hooks after ReverseProxy's Director and
// immediately before the actual upstream RoundTripper. Hook errors are
// returned to ReverseProxy so its existing ErrorHandler owns the response.
func WrapBeforeProxyHooks(next http.RoundTripper) http.RoundTripper {
	return &beforeProxyHookRoundTripper{next: next}
}

func (t *beforeProxyHookRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := runBeforeProxyHooks(r); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(r)
}

func isRuntimePluginPhase(phase Phase) bool {
	switch phase {
	case PhaseRewrite, PhaseConsumerRewrite, PhaseAccess, PhaseBeforeProxy,
		PhaseHeaderFilter, PhaseBodyFilter, PhaseLog, PhaseFinalizer, PhaseProtocol:
		return true
	default:
		return false
	}
}

func mergeEffectiveBindingSet(static, resolved []Binding) EffectiveBindingSet {
	global := make([]Binding, 0, len(static)+len(resolved))
	merged := make([]Binding, 0, len(static)+len(resolved))
	indexes := make(map[string]int)
	for _, binding := range static {
		if binding.Plugin == nil {
			continue
		}
		if binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal {
			global = append(global, binding)
			continue
		}
		appendEffectiveBinding(&merged, indexes, binding)
	}
	for _, binding := range resolved {
		if binding.Plugin == nil || binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal {
			continue
		}
		// Consumer and group winners execute in the same merged scope as
		// route bindings. Keep their exact provenance while allowing one
		// stage-local priority sort to order all effective winners.
		binding.Scope = ScopeRoute
		appendEffectiveBinding(&merged, indexes, binding)
	}
	return EffectiveBindingSet{global: global, merged: merged}
}

func appendEffectiveBinding(bindings *[]Binding, indexes map[string]int, binding Binding) {
	key := binding.Descriptor.Factory
	if key == "" {
		*bindings = append(*bindings, binding)
		return
	}
	if index, ok := indexes[key]; ok {
		(*bindings)[index] = binding
		return
	}
	indexes[key] = len(*bindings)
	*bindings = append(*bindings, binding)
}

func cloneBindings(bindings []Binding) []Binding {
	if bindings == nil {
		return nil
	}
	cloned := append([]Binding(nil), bindings...)
	for index := range cloned {
		cloned[index].Descriptor.Phases = append([]Phase(nil), bindings[index].Descriptor.Phases...)
		cloned[index].Descriptor.Scopes = append([]Scope(nil), bindings[index].Descriptor.Scopes...)
		cloned[index].Descriptor.response.Owners = append(
			[]ResponseOwnerKind(nil),
			bindings[index].Descriptor.response.Owners...,
		)
	}
	return cloned
}

// resolveBindingsForPlan freezes already-resolved construction inputs.
// Descriptor resolution belongs to plugin materialization, never plan building
// or request-local response compatibility checks.
func resolveBindingsForPlan(bindings []Binding) ([]Binding, error) {
	resolved := cloneBindings(bindings)
	for _, binding := range resolved {
		if binding.Plugin == nil {
			return nil, fmt.Errorf(
				"plugin plan binding has nil plugin (factory=%q resource=%s/%s)",
				binding.Descriptor.Factory,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
		if !binding.Descriptor.resolved {
			return nil, fmt.Errorf(
				"plugin plan binding has no resolved descriptor (factory=%q resource=%s/%s)",
				binding.Descriptor.Factory,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
	}
	return resolved, nil
}

// BindPluginChecked is the strict production constructor. It records the
// exact factory key and writes any config-derived request stage into the
// immutable Binding before it can enter a pipeline.
func BindPluginChecked(
	factoryName string,
	p Plugin,
	scope Scope,
	provenance ResourceProvenance,
) (Binding, error) {
	if factoryName == "" {
		return Binding{}, fmt.Errorf(
			"checked plugin binding rejected empty factory (resource=%s/%s)",
			provenance.Kind,
			provenance.ID,
		)
	}
	if p == nil {
		return Binding{}, fmt.Errorf(
			"checked plugin binding rejected nil plugin (factory=%q resource=%s/%s)",
			factoryName,
			provenance.Kind,
			provenance.ID,
		)
	}
	if _, ok := pluginRegistry[factoryName]; !ok {
		return Binding{}, fmt.Errorf(
			"checked plugin binding rejected unknown factory %q (resource=%s/%s)",
			factoryName,
			provenance.Kind,
			provenance.ID,
		)
	}
	descriptor, err := descriptorForRuntimeFactory(factoryName, p)
	if err != nil {
		return Binding{}, fmt.Errorf(
			"checked plugin binding rejected factory=%q resource=%s/%s: %w",
			factoryName,
			provenance.Kind,
			provenance.ID,
			err,
		)
	}
	return BindResolvedPlugin(
		descriptor,
		p,
		scope,
		provenance,
		InstanceIdentityInput{PluginConfig: p.Config()},
	)
}

// BindResolvedPlugin constructs a binding from the descriptor resolved once
// during plugin materialization. Wrappers may preserve callbacks, but cannot
// re-read config to change the immutable phase selection.
func BindResolvedPlugin(
	descriptor Descriptor,
	p Plugin,
	scope Scope,
	provenance ResourceProvenance,
	identity InstanceIdentityInput,
) (Binding, error) {
	return bindResolvedPlugin(0, descriptor, p, scope, provenance, identity)
}

// BindGenerationResolvedPlugin constructs a generation-owned binding.
func BindGenerationResolvedPlugin(
	generation uint64,
	descriptor Descriptor,
	p Plugin,
	scope Scope,
	provenance ResourceProvenance,
	identity InstanceIdentityInput,
) (Binding, error) {
	if generation == 0 {
		return Binding{}, fmt.Errorf("resolved plugin binding %q has no generation", descriptor.Factory)
	}
	return bindResolvedPlugin(generation, descriptor, p, scope, provenance, identity)
}

func bindResolvedPlugin(
	generation uint64,
	descriptor Descriptor,
	p Plugin,
	scope Scope,
	provenance ResourceProvenance,
	identity InstanceIdentityInput,
) (Binding, error) {
	if p == nil {
		return Binding{}, fmt.Errorf("resolved plugin binding %q has nil plugin", descriptor.Factory)
	}
	if !descriptor.resolved || descriptor.Factory == "" {
		return Binding{}, fmt.Errorf("plugin descriptor %q is not resolved", descriptor.Factory)
	}
	if !slices.Contains(descriptor.Scopes, scope) {
		return Binding{}, fmt.Errorf(
			"plugin descriptor %q rejects scope %d (resource=%s/%s allowed=%v)",
			descriptor.Factory,
			scope,
			provenance.Kind,
			provenance.ID,
			descriptor.Scopes,
		)
	}
	descriptor.Phases = append([]Phase(nil), descriptor.Phases...)
	descriptor.Scopes = append([]Scope(nil), descriptor.Scopes...)
	descriptor.response.Owners = append([]ResponseOwnerKind(nil), descriptor.response.Owners...)
	logPolicy := base.LogCapturePolicy{}
	if provider, ok := p.(base.LogCapturePolicyPlugin); ok {
		logPolicy = provider.LogCapturePolicy()
	}
	if err := base.ValidateLogCapturePolicy(logPolicy); err != nil {
		return Binding{}, fmt.Errorf(
			"resolved plugin binding %q has invalid log capture policy: %w",
			descriptor.Factory,
			err,
		)
	}
	key, err := newInstanceKey(generation, descriptor, scope, provenance, identity)
	if err != nil {
		return Binding{}, err
	}
	return Binding{
		Plugin:       p,
		Descriptor:   descriptor,
		Priority:     descriptor.Priority,
		Scope:        scope,
		Provenance:   provenance,
		InstanceKey:  key,
		logPolicy:    logPolicy,
		logPolicySet: true,
	}, nil
}

func terminalHandler(terminal http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			lifecycle := apisixctx.GetRequestLifecycle(r)
			if lifecycle == nil {
				return
			}
			lifecycle.SetFinalRequest(r)
		}()
		terminal.ServeHTTP(w, r)
	})
}

func writeStableResponseError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":"%s"}`, message)
}

func markResponseInternalFailure(r *http.Request) {
	if r == nil {
		return
	}
	if execution, ok := r.Context().Value(responseExecutionKey{}).(*responseExecution); ok && execution != nil {
		execution.internalFailure = true
	}
}

func responseFailureRequiresAbort(r *http.Request) bool {
	if r == nil {
		return false
	}
	execution, ok := r.Context().Value(responseExecutionKey{}).(*responseExecution)
	return ok && execution != nil && execution.transparentCommitted
}
