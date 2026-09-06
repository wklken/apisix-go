package plugin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestPreferRouteDisabledConsumerRestoresGlobalLog(t *testing.T) {
	global := newLogExecutorTestPlugin("global-prometheus", 1, nil)
	route := newLogExecutorTestPlugin("route-prometheus", 2, nil)
	bindings := []Binding{
		bindPluginForTest("prometheus", global, ScopeGlobal, ResourceProvenance{
			Kind: ResourceGlobalRule, ID: "global-1",
		}),
		bindPluginForTest("prometheus", route, ScopeRoute, ResourceProvenance{
			Kind: ResourceRoute, ID: "route-1",
		}),
	}
	logExecutor, err := NewLogExecutorFromBindings(bindings)
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	pipeline := NewRequestPipeline(bindings, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{
			Request:           r,
			OverrideFactories: []string{"prometheus"},
			Resolved:          true,
		}, nil
	}).WithLogExecutor(&logExecutor)

	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	recorder := httptest.NewRecorder()
	wrapped, capture := base.CaptureResponseOutcomeController(recorder)
	request = base.WithResponseCapture(request, capture)
	pipeline.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(wrapped, request)
	lifecycle.Complete(capture.Outcome(), time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	t.Logf("callbacks: global=%d route=%d", len(global.seen), len(route.seen))
	if len(global.seen) != 1 || len(route.seen) != 0 {
		t.Fatalf("callbacks: global=%d route=%d, want global=1 route=0", len(global.seen), len(route.seen))
	}
}
