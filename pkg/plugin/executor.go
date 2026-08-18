package plugin

import (
	"cmp"
	"fmt"
	"io"
	"net/http"
	"slices"

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
	RequestStageLegacy RequestStage = iota
	RequestStageRewrite
	RequestStageConsumerRewrite
	RequestStageAccess
	RequestStageBeforeProxy
	RequestStageNone
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
// factoryName is intentionally private: callers must provide the exact
// registry/config key through BindPlugin rather than deriving it from GetName.
type Binding struct {
	Plugin     Plugin
	Scope      Scope
	Stage      RequestStage
	Provenance ResourceProvenance

	factoryName string
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
}

type preparedStaticPipeline struct {
	effective EffectiveBindingSet
	handler   http.Handler
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
	globalRewrite := make([]Binding, 0)
	systemRewrite := make([]Binding, 0)
	for _, binding := range p.bindings {
		if binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal || binding.Scope == ScopeRoute {
			if binding.factoryName == "cors" {
				if binding.Plugin != nil {
					corsBindings = append(corsBindings, binding)
				}
				continue
			}
			spec, ok := RequestStageFor(binding.factoryName)
			if ok && spec.AuthenticatesConsumer {
				bindings = append(bindings, binding)
				continue
			}
			if ok && binding.Stage == RequestStageRewrite {
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
	next = wrapRequestStageBindings(next, globalRewrite)
	next = wrapRequestStageBindings(next, systemRewrite)
	return next
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var beforeAuthentication http.Header
		afterAuthentication := http.HandlerFunc(func(provisional http.ResponseWriter, r *http.Request) {
			// Authentication succeeded. Preserve only the header changes made
			// by authentication; provisional static CORS headers are discarded
			// so the post-resolution winner owns the response.
			applyHeaderDelta(w.Header(), beforeAuthentication, provisional.Header())
			next.ServeHTTP(w, r)
		})
		authentication := wrapRequestStageBindings(afterAuthentication, authBindings)
		handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			beforeAuthentication = w.Header().Clone()
			authentication.ServeHTTP(w, r)
		}))
		for _, binding := range ordered {
			if binding.Plugin != nil {
				handler = binding.Plugin.Handler(handler)
			}
		}
		handler.ServeHTTP(provisionalResponseWriter(w), r)
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
	bindings := effective.all()
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
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			status := http.StatusInternalServerError
			message := "Internal Server Error"
			if base.IsBodyTooLarge(err) {
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
	legacy := legacyRemainderBindings(bindings)
	if len(legacy) > 0 {
		legacyHandler := Executor{bindings: legacy}.thenLegacy(handler, legacy)
		transformCount := 0
		for _, binding := range legacy {
			if binding.Plugin != nil && isResponseTransformPlugin(binding.Plugin.GetName()) {
				transformCount++
			}
		}
		handler = base.WithTransformPipeline(transformCount)(legacyHandler)
	}
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
	slices.SortStableFunc(ordered, func(a, b Binding) int {
		if a.Plugin == nil || b.Plugin == nil {
			return 0
		}
		return cmp.Compare(b.Plugin.GetPriority(), a.Plugin.GetPriority())
	})
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
		if binding.Scope != scope || binding.Stage != stage || binding.Plugin == nil {
			continue
		}
		if spec, ok := RequestStageFor(binding.factoryName); ok && spec.AuthenticatesConsumer {
			continue
		}
		selected = append(selected, binding)
	}
	return wrapRequestStageBindings(next, selected)
}

func requestStageHandler(binding Binding, next http.Handler) http.Handler {
	if phase, ok := binding.Plugin.(base.RequestPhasePlugin); ok {
		return base.AdaptRequestPhase(phase, next)
	}
	if spec, ok := RequestStageFor(binding.factoryName); ok && spec.AdaptLegacyHandler {
		return base.AdaptRequestPhase(
			newRewriteOnlyAdapter(binding.factoryName, binding.Plugin, binding.Provenance),
			next,
		)
	}
	return base.AdaptRequestPhase(newUnregisteredRewriteAdapter(binding.factoryName, binding.Provenance), next)
}

func legacyRemainderBindings(bindings []Binding) []Binding {
	legacy := make([]Binding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Plugin == nil || binding.Stage != RequestStageLegacy {
			continue
		}
		if spec, ok := RequestStageFor(binding.factoryName); ok && spec.AuthenticatesConsumer {
			continue
		}
		legacy = append(legacy, binding)
	}
	slices.SortStableFunc(legacy, func(a, b Binding) int {
		return cmp.Compare(b.Plugin.GetPriority(), a.Plugin.GetPriority())
	})
	return legacy
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
	key := binding.factoryName
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
	return append([]Binding(nil), bindings...)
}

// BindPlugin records the exact factory name and resolves its audited request
// stage. Unknown names remain in the legacy remainder for compatibility.
func BindPlugin(
	factoryName string,
	p Plugin,
	scope Scope,
	provenance ResourceProvenance,
) Binding {
	stage := RequestStageLegacy
	if spec, ok := RequestStageFor(factoryName); ok {
		stage = spec.Stage
	}
	return Binding{
		Plugin:      p,
		Scope:       scope,
		Stage:       stage,
		Provenance:  provenance,
		factoryName: factoryName,
	}
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
	spec, ok := RequestStageFor(factoryName)
	if !ok {
		return Binding{}, fmt.Errorf(
			"checked plugin binding has no request owner (factory=%q resource=%s/%s)",
			factoryName,
			provenance.Kind,
			provenance.ID,
		)
	}
	if spec.ConfigAware {
		resolved, _, err := ResolveRequestStage(factoryName, p.Config())
		if err != nil {
			return Binding{}, fmt.Errorf(
				"checked plugin binding rejected factory=%q resource=%s/%s: %w",
				factoryName,
				provenance.Kind,
				provenance.ID,
				err,
			)
		}
		spec = resolved
	}
	return Binding{
		Plugin:      p,
		Scope:       scope,
		Stage:       spec.Stage,
		Provenance:  provenance,
		factoryName: factoryName,
	}, nil
}

// Executor owns an immutable snapshot of either a legacy plugin list or
// scoped bindings, plus the response transform count captured at build time.
type Executor struct {
	bindings       []Binding
	scoped         bool
	transformCount int
}

func NewExecutor(plugins ...Plugin) Executor {
	sorted := append([]Plugin(nil), plugins...)
	slices.SortFunc(sorted, func(a, b Plugin) int {
		return cmp.Compare(b.GetPriority(), a.GetPriority())
	})

	transformCount := 0
	for _, plugin := range sorted {
		if isResponseTransformPlugin(plugin.GetName()) {
			transformCount++
		}
	}
	bindings := make([]Binding, len(sorted))
	for i, plugin := range sorted {
		bindings[i] = Binding{Plugin: plugin}
	}
	return Executor{bindings: bindings, transformCount: transformCount}
}

// NewScopedExecutor clones the supplied bindings and executes only system and
// global audited rewrite stages by scope while retaining route rewrites and
// every other plugin in the legacy priority chain until the auth/consumer
// boundary is migrated.
func NewScopedExecutor(bindings ...Binding) Executor {
	cloned := append([]Binding(nil), bindings...)
	transformCount := 0
	for _, binding := range cloned {
		if binding.Plugin != nil && isResponseTransformPlugin(binding.Plugin.GetName()) {
			transformCount++
		}
	}
	return Executor{bindings: cloned, scoped: true, transformCount: transformCount}
}

func (e Executor) Then(terminal http.Handler) http.Handler {
	handler := terminalHandler(terminal)
	if !e.scoped {
		return base.WithTransformPipeline(e.transformCount)(e.thenLegacy(handler, e.bindings))
	}

	legacy := e.legacyBindings()
	handler = e.thenLegacy(handler, legacy)
	for _, scope := range []Scope{ScopeGlobal, ScopeSystem} {
		handler = e.thenScopedRewrite(handler, scope)
	}
	return base.WithTransformPipeline(e.transformCount)(handler)
}

func (e Executor) thenLegacy(handler http.Handler, bindings []Binding) http.Handler {
	for _, current := range slices.Backward(bindings) {
		if current.Plugin == nil {
			continue
		}
		if phase, ok := current.Plugin.(base.RequestPhasePlugin); ok {
			handler = base.AdaptRequestPhase(phase, handler)
			continue
		}
		handler = current.Plugin.Handler(handler)
	}
	return handler
}

func (e Executor) legacyBindings() []Binding {
	legacy := make([]Binding, 0, len(e.bindings))
	for _, binding := range e.bindings {
		if binding.Plugin == nil || e.isScopedRewrite(binding) {
			continue
		}
		legacy = append(legacy, binding)
	}
	slices.SortStableFunc(legacy, func(a, b Binding) int {
		return cmp.Compare(b.Plugin.GetPriority(), a.Plugin.GetPriority())
	})
	return legacy
}

func (e Executor) isScopedRewrite(binding Binding) bool {
	if binding.Stage != RequestStageRewrite {
		return false
	}
	switch binding.Scope {
	case ScopeSystem, ScopeGlobal:
		return true
	default:
		return false
	}
}

func (e Executor) thenScopedRewrite(next http.Handler, scope Scope) http.Handler {
	rewrites := make([]Binding, 0, len(e.bindings))
	for _, binding := range e.bindings {
		if binding.Scope != scope || !e.isScopedRewrite(binding) {
			continue
		}
		rewrites = append(rewrites, binding)
	}
	slices.SortStableFunc(rewrites, func(a, b Binding) int {
		return cmp.Compare(b.Plugin.GetPriority(), a.Plugin.GetPriority())
	})
	for _, binding := range slices.Backward(rewrites) {
		next = e.scopedRewriteHandler(binding, next)
	}
	return next
}

func (e Executor) scopedRewriteHandler(binding Binding, next http.Handler) http.Handler {
	if spec, ok := RequestStageFor(binding.factoryName); ok {
		if spec.AdaptLegacyHandler {
			return base.AdaptRequestPhase(
				newRewriteOnlyAdapter(binding.factoryName, binding.Plugin, binding.Provenance),
				next,
			)
		}
		if phase, ok := binding.Plugin.(base.RequestPhasePlugin); ok {
			return base.AdaptRequestPhase(phase, next)
		}
		return base.AdaptRequestPhase(newUnregisteredRewriteAdapter(binding.factoryName, binding.Provenance), next)
	}
	if phase, ok := binding.Plugin.(base.RequestPhasePlugin); ok {
		return base.AdaptRequestPhase(phase, next)
	}
	return base.AdaptRequestPhase(newUnregisteredRewriteAdapter(binding.factoryName, binding.Provenance), next)
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
