package route

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
)

type requestPhaseMetadataPlugin struct {
	name         string
	priority     int
	phaseCalls   *int
	handlerCalls *int
	order        *[]string
	status       int
	stop         bool
}

func (p *requestPhaseMetadataPlugin) Init() error               { return nil }
func (p *requestPhaseMetadataPlugin) PostInit() error           { return nil }
func (p *requestPhaseMetadataPlugin) Config() any               { return nil }
func (p *requestPhaseMetadataPlugin) GetSchema() string         { return "" }
func (p *requestPhaseMetadataPlugin) GetMetadataSchema() string { return "" }
func (p *requestPhaseMetadataPlugin) GetPriority() int          { return p.priority }
func (p *requestPhaseMetadataPlugin) GetName() string           { return p.name }

func (p *requestPhaseMetadataPlugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.handlerCalls != nil {
			(*p.handlerCalls)++
		}
		if p.order != nil {
			*p.order = append(*p.order, p.name+":handler")
		}
		next.ServeHTTP(w, r)
	})
}

func (p *requestPhaseMetadataPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	if p.phaseCalls != nil {
		*p.phaseCalls++
	}
	if p.order != nil {
		*p.order = append(*p.order, p.name+":phase")
	}
	if p.status != 0 {
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte("original"))
	}
	if p.stop {
		return base.StopRequest(r)
	}
	return base.ContinueRequest(r)
}

var (
	_ plugin.Plugin           = (*requestPhaseMetadataPlugin)(nil)
	_ base.RequestPhasePlugin = (*requestPhaseMetadataPlugin)(nil)
)

type requestPhaseMetadataLegacyPlugin struct {
	name     string
	priority int
	order    *[]string
}

func (p *requestPhaseMetadataLegacyPlugin) Init() error               { return nil }
func (p *requestPhaseMetadataLegacyPlugin) PostInit() error           { return nil }
func (p *requestPhaseMetadataLegacyPlugin) Config() any               { return nil }
func (p *requestPhaseMetadataLegacyPlugin) GetSchema() string         { return "" }
func (p *requestPhaseMetadataLegacyPlugin) GetMetadataSchema() string { return "" }
func (p *requestPhaseMetadataLegacyPlugin) GetPriority() int          { return p.priority }
func (p *requestPhaseMetadataLegacyPlugin) GetName() string           { return p.name }

func (p *requestPhaseMetadataLegacyPlugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*p.order = append(*p.order, p.name+":enter")
		next.ServeHTTP(w, r)
		*p.order = append(*p.order, p.name+":exit")
	})
}

var _ plugin.Plugin = (*requestPhaseMetadataLegacyPlugin)(nil)

func TestRequestPhaseMetadataContract(t *testing.T) {
	t.Run("filter false skips explicit phase and continues", func(t *testing.T) {
		phaseCalls := 0
		terminalCalls := 0
		p := &requestPhaseMetadataPlugin{name: "filtered", phaseCalls: &phaseCalls}
		filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
		if err != nil {
			t.Fatalf("compile filter: %v", err)
		}
		wrapped := metadataRequestPlugin{
			Plugin: p,
			phase:  p,
			filter: filter,
		}
		handler := plugin.NewExecutor(wrapped).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			terminalCalls++
			w.WriteHeader(http.StatusNoContent)
		}))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?enabled=no", nil))
		if phaseCalls != 0 || terminalCalls != 1 || response.Code != http.StatusNoContent {
			t.Fatalf(
				"phase calls = %d, terminal calls = %d, status = %d; want 0, 1, 204",
				phaseCalls,
				terminalCalls,
				response.Code,
			)
		}
	})

	t.Run("filter true executes explicit phase", func(t *testing.T) {
		phaseCalls := 0
		terminalCalls := 0
		p := &requestPhaseMetadataPlugin{name: "filtered", phaseCalls: &phaseCalls}
		filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
		if err != nil {
			t.Fatalf("compile filter: %v", err)
		}
		wrapped := metadataRequestPlugin{Plugin: p, phase: p, filter: filter}
		handler := plugin.NewExecutor(wrapped).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			terminalCalls++
			w.WriteHeader(http.StatusNoContent)
		}))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?enabled=yes", nil))
		if phaseCalls != 1 || terminalCalls != 1 || response.Code != http.StatusNoContent {
			t.Fatalf(
				"phase calls = %d, terminal calls = %d, status = %d; want 1, 1, 204",
				phaseCalls,
				terminalCalls,
				response.Code,
			)
		}
	})

	t.Run("priority crosses a legacy plugin", func(t *testing.T) {
		order := []string{}
		high := &requestPhaseMetadataPlugin{name: "explicit-high", priority: 300, order: &order}
		legacy := &requestPhaseMetadataLegacyPlugin{name: "legacy", priority: 200, order: &order}
		low := &requestPhaseMetadataPlugin{name: "explicit-low", priority: 100, order: &order}
		handler := assembleRoutePluginChain(
			[]plugin.Plugin{high, legacy, low},
			nil,
		).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			order = append(order, "terminal")
			w.WriteHeader(http.StatusNoContent)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		want := []string{"explicit-high:phase", "legacy:enter", "explicit-low:phase", "terminal", "legacy:exit"}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("order = %#v, want %#v", order, want)
		}
	})

	t.Run("error response replaces only explicit phase writes", func(t *testing.T) {
		phaseCalls := 0
		terminalCalls := 0
		p := &requestPhaseMetadataPlugin{
			name:       "explicit-error",
			phaseCalls: &phaseCalls,
			status:     http.StatusUnauthorized,
			stop:       true,
		}
		wrapped := metadataRequestPlugin{
			Plugin:        p,
			phase:         p,
			errorResponse: map[string]any{"message": "custom"},
		}
		handler := plugin.NewExecutor(wrapped).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			terminalCalls++
			w.WriteHeader(http.StatusNoContent)
		}))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if phaseCalls != 1 || terminalCalls != 0 {
			t.Fatalf("phase calls = %d, terminal calls = %d; want 1, 0", phaseCalls, terminalCalls)
		}
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
		}
		if got := strings.TrimSpace(response.Body.String()); got != `{"message":"custom"}` {
			t.Fatalf("body = %q, want custom response", got)
		}
	})

	t.Run("consumer override runs consumer phase once", func(t *testing.T) {
		routeCalls := 0
		consumerCalls := 0
		routePlugin := &requestPhaseMetadataPlugin{name: "synthetic-auth", priority: 200, phaseCalls: &routeCalls}
		consumerPlugin := &requestPhaseMetadataPlugin{name: "synthetic-auth", priority: 100, phaseCalls: &consumerCalls}
		routeExecutor := plugin.NewExecutor(newRouteConsumerOverridePlugin(routePlugin))
		consumerExecutor := plugin.NewExecutor(consumerPlugin)
		handler := routeExecutor.Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			consumerExecutor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(w, r)
		}))
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request = apisixctx.WithConsumerPluginOverrides(request, map[string]struct{}{"synthetic-auth": {}})
		handler.ServeHTTP(httptest.NewRecorder(), request)
		if routeCalls != 0 || consumerCalls != 1 {
			t.Fatalf("route calls = %d, consumer calls = %d; want 0, 1", routeCalls, consumerCalls)
		}
	})

	t.Run("direct Handler remains original path", func(t *testing.T) {
		handlerCalls := 0
		p := &requestPhaseMetadataPlugin{name: "direct", handlerCalls: &handlerCalls}
		wrapped := metadataRequestPlugin{Plugin: p, phase: p}
		wrapped.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if handlerCalls != 1 {
			t.Fatalf("original Handler calls = %d, want 1", handlerCalls)
		}
	})
}

func TestGlobalNotFoundHandlerRecordsEarlyStopSource(t *testing.T) {
	builder := NewBuilder(nil)
	handler, err := builder.buildGlobalNotFoundHandler(nil)
	if err != nil {
		t.Fatalf("buildGlobalNotFoundHandler() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/missing", nil),
		time.Unix(0, 0),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("response source = %q, want %q", got, apisixctx.ResponseSourceEarlyStop)
	}
}
