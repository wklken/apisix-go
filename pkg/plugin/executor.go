package plugin

import (
	"cmp"
	"net/http"
	"slices"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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
	RequestStageAccess
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
