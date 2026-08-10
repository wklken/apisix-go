package plugin

import (
	"cmp"
	"net/http"
	"slices"

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

// RequestPipeline owns the explicit Plan 14 request order. Static bindings
// are cloned at construction; resolution data is merged per request so no
// mutable request, auth, or override state can leak between requests.
type RequestPipeline struct {
	bindings []Binding
	resolve  ConsumerBindingResolver
}

func NewRequestPipeline(bindings []Binding, resolve ConsumerBindingResolver) RequestPipeline {
	return RequestPipeline{bindings: cloneBindings(bindings), resolve: resolve}
}

func (p RequestPipeline) Then(terminal http.Handler) http.Handler {
	if terminal == nil {
		terminal = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	}
	afterAuthentication := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.runResolved(w, r, terminal)
	})
	handler := p.wrapAuthentication(afterAuthentication)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	})
}

func (p RequestPipeline) wrapAuthentication(next http.Handler) http.Handler {
	bindings := make([]Binding, 0)
	globalRewrite := make([]Binding, 0)
	systemRewrite := make([]Binding, 0)
	for _, binding := range p.bindings {
		if binding.Scope == ScopeSystem || binding.Scope == ScopeGlobal || binding.Scope == ScopeRoute {
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
	next = wrapRequestStageBindings(next, bindings)
	next = wrapRequestStageBindings(next, globalRewrite)
	next = wrapRequestStageBindings(next, systemRewrite)
	return next
}

func (p RequestPipeline) runResolved(w http.ResponseWriter, r *http.Request, terminal http.Handler) {
	resolution := ConsumerResolution{Request: r}
	var err error
	if p.resolve != nil {
		resolution, err = p.resolve(r)
	}
	if err != nil {
		logger.Errorf("consumer binding resolution failed: %v", err)
		if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
			lifecycle.SetFinalRequest(r)
			lifecycle.SetResponseSource(apisixctx.ResponseSourceEarlyStop)
		}
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	request := resolution.Request
	if request == nil {
		request = r
	}
	effective := mergeEffectiveBindings(p.bindings, resolution.Bindings)
	inner := p.buildPostResolutionHandler(effective, terminal)
	inner.ServeHTTP(w, request)
}

func (p RequestPipeline) buildPostResolutionHandler(bindings []Binding, terminal http.Handler) http.Handler {
	boundary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.FinalizeProxyRewrite(r)
		apisixctx.RunBeforeProxyHooks(r)
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

func mergeEffectiveBindings(static, resolved []Binding) []Binding {
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
	return append(global, merged...)
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
			if lifecycle.ResponseSource() == apisixctx.ResponseSourceUnknown {
				lifecycle.SetResponseSource(apisixctx.ResponseSourceUpstream)
			}
		}()
		terminal.ServeHTTP(w, r)
	})
}
