package route

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type routeBufferedPlugin struct {
	name         string
	descriptor   base.BindingPhaseDescriptor
	requestStop  bool
	requestCalls int
	headerCalls  int
	bodyCalls    int
	storeCalls   int
	storeErr     error
	bodySuffix   string
	events       *[]string
	hitState     base.CachedResponseState
	publishHit   bool
}

func (p *routeBufferedPlugin) Init() error               { return nil }
func (p *routeBufferedPlugin) PostInit() error           { return nil }
func (p *routeBufferedPlugin) GetSchema() string         { return "" }
func (p *routeBufferedPlugin) GetMetadataSchema() string { return "" }
func (p *routeBufferedPlugin) GetPriority() int          { return 100 }
func (p *routeBufferedPlugin) GetName() string           { return p.name }
func (p *routeBufferedPlugin) Handler(next http.Handler) http.Handler {
	return next
}
func (p *routeBufferedPlugin) Config() any { return p }
func (p *routeBufferedPlugin) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	return p.descriptor, nil
}

func (p *routeBufferedPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	p.requestCalls++
	if p.publishHit {
		if holder := base.CacheHitResponseHolderFromRequest(r); holder != nil {
			holder.Publish(p.hitState)
		}
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceCacheHit)
	}
	if p.requestStop {
		_, _ = w.Write([]byte("auth"))
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	return base.ContinueRequest(r)
}

func (p *routeBufferedPlugin) RunHeaderFilter(_ *http.Request, state *base.ResponseState) error {
	p.headerCalls++
	state.Header.Set("X-Route-Header", "set")
	return nil
}

func (p *routeBufferedPlugin) RunBufferedBodyFilter(
	_ *http.Request,
	state *base.ResponseState,
) error {
	p.bodyCalls++
	if p.events != nil {
		*p.events = append(*p.events, "body")
	}
	state.Body = append(state.Body, []byte(p.bodySuffix)...)
	return nil
}

func (p *routeBufferedPlugin) RunFinalResponseStore(_ *http.Request, _ base.ResponseState) error {
	p.storeCalls++
	if p.events != nil {
		*p.events = append(*p.events, "store")
	}
	return p.storeErr
}

func (p *routeBufferedPlugin) AppliesToResponseSource(source apisixctx.ResponseSource) bool {
	return source == apisixctx.ResponseSourceUpstream ||
		source == apisixctx.ResponseSourceAPISIX ||
		source == apisixctx.ResponseSourceEarlyStop
}

func checkedRouteBinding(
	t *testing.T,
	name string,
	p *routeBufferedPlugin,
	scope plugin.Scope,
) plugin.Binding {
	t.Helper()
	binding, err := plugin.BindPluginChecked(
		name,
		p,
		scope,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: name},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked(%q) error = %v", name, err)
	}
	return binding
}

func serveBufferedRoute(
	t *testing.T,
	static []plugin.Binding,
	resolve plugin.ConsumerBindingResolver,
	terminal http.Handler,
) *httptest.ResponseRecorder {
	t.Helper()
	executor, err := plugin.NewBufferedResponseExecutor(
		static,
		plugin.TerminalDescriptor{Owner: plugin.TerminalOwnerOrdinaryProxy},
		base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	)
	if err != nil {
		t.Fatalf("NewBufferedResponseExecutor() error = %v", err)
	}
	handler := plugin.NewRequestPipeline(static, resolve).
		WithBufferedResponseExecutor(executor).
		Then(terminal)
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(0, 0),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestResolvedConsumerResponseWinnerMaterializesBeforeRouteStageAndRunsOnce(t *testing.T) {
	routePlugin := &routeBufferedPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
		bodySuffix: "-route",
	}
	consumerPlugin := &routeBufferedPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
		bodySuffix: "-consumer",
	}
	static := []plugin.Binding{checkedRouteBinding(t, "echo", routePlugin, plugin.ScopeRoute)}
	consumer := checkedRouteBinding(t, "echo", consumerPlugin, plugin.ScopeConsumer)
	response := serveBufferedRoute(
		t,
		static,
		func(r *http.Request) (plugin.ConsumerResolution, error) {
			return plugin.ConsumerResolution{
				Request:  r,
				Resolved: true,
				Bindings: []plugin.Binding{consumer},
			}, nil
		},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			_, _ = w.Write([]byte("base"))
		}),
	)
	if got := response.Body.String(); got != "base-consumer" {
		t.Fatalf("body = %q, want consumer winner exactly once", got)
	}
	if routePlugin.bodyCalls != 0 || consumerPlugin.bodyCalls != 1 {
		t.Fatalf(
			"route/consumer body calls = %d/%d, want 0/1",
			routePlugin.bodyCalls,
			consumerPlugin.bodyCalls,
		)
	}
}

func TestAuthStopUsesStaticPlanWithoutConsumerBinding(t *testing.T) {
	staticPlugin := &routeBufferedPlugin{
		name:        "body-transformer",
		descriptor:  base.BindingPhaseDescriptor{RequestStage: "rewrite", BufferedBody: true},
		requestStop: true,
		bodySuffix:  "-static",
	}
	static := []plugin.Binding{
		checkedRouteBinding(t, "body-transformer", staticPlugin, plugin.ScopeRoute),
	}
	response := serveBufferedRoute(
		t,
		static,
		func(r *http.Request) (plugin.ConsumerResolution, error) {
			return plugin.ConsumerResolution{Request: r, Resolved: true}, nil
		},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("terminal called after auth stop")
		}),
	)
	if staticPlugin.requestCalls != 1 || staticPlugin.bodyCalls != 1 {
		t.Fatalf(
			"request/body calls = %d/%d, want 1/1",
			staticPlugin.requestCalls,
			staticPlugin.bodyCalls,
		)
	}
	if response.Code != http.StatusOK || response.Body.String() != "auth-static" {
		t.Fatalf(
			"auth-stop response = %d/%q, want 200/auth-static",
			response.Code,
			response.Body.String(),
		)
	}
}

func TestDynamicBoundedWinnerRejectsConflictBeforeTerminal(t *testing.T) {
	responsePlugin := &routeBufferedPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
	}
	responseBinding := checkedRouteBinding(t, "echo", responsePlugin, plugin.ScopeConsumer)
	conflict := bindPluginForTest("gzip", &routeBufferedPlugin{name: "gzip"}, plugin.ScopeConsumer,
		plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "conflict"})
	terminalCalls := 0
	response := serveBufferedRoute(
		t,
		nil,
		func(r *http.Request) (plugin.ConsumerResolution, error) {
			return plugin.ConsumerResolution{
				Request:  r,
				Resolved: true,
				Bindings: []plugin.Binding{responseBinding, conflict},
			}, nil
		},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			terminalCalls++
		}),
	)
	if terminalCalls != 0 || response.Code != http.StatusInternalServerError {
		t.Fatalf(
			"terminal/status = %d/%d, want conflict before terminal and stable 500",
			terminalCalls,
			response.Code,
		)
	}
}

func TestBufferedRouteCacheHitConsumesOnceAndSkipsTransformsStores(t *testing.T) {
	cache := &routeBufferedPlugin{
		name:       "proxy-cache",
		publishHit: true,
		hitState: base.CachedResponseState{
			Status: http.StatusAccepted,
			Header: http.Header{"X-Cache": {"hit"}},
			Body:   []byte("cached"),
		},
	}
	response := serveBufferedRoute(
		t,
		[]plugin.Binding{checkedRouteBinding(t, "proxy-cache", cache, plugin.ScopeRoute)},
		nil,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("terminal called on cache hit")
		}),
	)
	if response.Code != http.StatusAccepted || response.Body.String() != "cached" {
		t.Fatalf("cache response = %d/%q, want 202/cached", response.Code, response.Body.String())
	}
	if cache.storeCalls != 0 || cache.bodyCalls != 0 {
		t.Fatalf("cache transforms/stores = %d/%d, want 0/0", cache.bodyCalls, cache.storeCalls)
	}
}

func TestBufferedRouteCacheMissStoresAfterTransformsPerInstance(t *testing.T) {
	cache := &routeBufferedPlugin{name: "proxy-cache", bodySuffix: "-transformed"}
	response := serveBufferedRoute(
		t,
		[]plugin.Binding{checkedRouteBinding(t, "proxy-cache", cache, plugin.ScopeRoute)},
		nil,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			_, _ = w.Write([]byte("origin"))
		}),
	)
	if response.Body.String() != "origin" || cache.storeCalls != 1 {
		t.Fatalf(
			"cache miss body/stores = %q/%d, want origin/1",
			response.Body.String(),
			cache.storeCalls,
		)
	}
}

func TestBufferedRouteFinalStoreErrorCommitsUnchangedOnce(t *testing.T) {
	cache := &routeBufferedPlugin{name: "proxy-cache", storeErr: errors.New("store failed")}
	response := serveBufferedRoute(
		t,
		[]plugin.Binding{checkedRouteBinding(t, "proxy-cache", cache, plugin.ScopeRoute)},
		nil,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			w.Header().Set("X-Origin", "yes")
			_, _ = w.Write([]byte("unchanged"))
		}),
	)
	if response.Body.String() != "unchanged" || response.Header().Get("X-Origin") != "yes" ||
		cache.storeCalls != 1 {
		t.Fatalf(
			"store-error response = %q/%q/stores=%d, want unchanged/yes/1",
			response.Body.String(),
			response.Header().Get("X-Origin"),
			cache.storeCalls,
		)
	}
	if strings.Contains(response.Body.String(), "Internal Server Error") {
		t.Fatal("returned store error replaced the client response")
	}
}
