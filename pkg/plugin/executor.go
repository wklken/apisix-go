package plugin

import (
	"cmp"
	"net/http"
	"slices"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// Executor owns a priority-sorted snapshot of plugins and the response
// transform count captured when that snapshot was built.
type Executor struct {
	plugins        []Plugin
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
	return Executor{plugins: sorted, transformCount: transformCount}
}

func (e Executor) Then(terminal http.Handler) http.Handler {
	handler := terminalHandler(terminal)
	for _, current := range slices.Backward(e.plugins) {
		if phase, ok := current.(base.RequestPhasePlugin); ok {
			handler = base.AdaptRequestPhase(phase, handler)
			continue
		}
		handler = current.Handler(handler)
	}
	return base.WithTransformPipeline(e.transformCount)(handler)
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
