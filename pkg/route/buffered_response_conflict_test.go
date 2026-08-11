package route

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

type routeOptionalWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *routeOptionalWriter) Header() http.Header { return w.header }
func (w *routeOptionalWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *routeOptionalWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(body)
}

func (w *routeOptionalWriter) ReadFrom(reader io.Reader) (int64, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return io.Copy(&w.body, reader)
}
func (*routeOptionalWriter) Flush() {}
func (*routeOptionalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errRouteOptionalUnsupported
}

func (*routeOptionalWriter) Push(
	string,
	*http.PushOptions,
) error {
	return errRouteOptionalUnsupported
}

var errRouteOptionalUnsupported = &routeOptionalError{}

type routeOptionalError struct{}

func (*routeOptionalError) Error() string { return "optional operation unsupported" }

func TestNoBoundedPlanPreservesFlushHijackPushReaderFromAndAIAssembly(t *testing.T) {
	t.Run("production route assembly", func(t *testing.T) {
		ensureRouteStore(t)
		const terminalHeader = "reached-next"
		receivedTerminalHeader := make(chan string, 1)
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedTerminalHeader <- r.Header.Get("X-Plan15-Terminal")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[]}`))
		}))
		t.Cleanup(provider.Close)
		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.buildHandlerStrict(resource.Route{
			ID: "no-bounded-ai-route",
			Plugins: map[string]resource.PluginConfig{
				"ai-proxy": map[string]any{
					"provider": "openai-compatible",
					"auth":     map[string]any{},
					"override": map[string]any{"endpoint": provider.URL},
				},
				"proxy-rewrite": map[string]any{
					"headers": map[string]any{
						"set": map[string]any{"X-Plan15-Terminal": terminalHeader},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("buildHandlerStrict() error = %v", err)
		}
		request := httptest.NewRequest(
			http.MethodPost,
			"http://gateway.test/ai",
			bytes.NewReader([]byte(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`)),
		)
		request.Header.Set("Content-Type", "application/json")
		writer := &routeOptionalWriter{header: make(http.Header)}
		handler.ServeHTTP(writer, request)
		if writer.status != http.StatusOK || writer.body.String() != `{"choices":[]}` {
			t.Fatalf("production AI response = %d/%q, want 200/provider body", writer.status, writer.body.String())
		}
		if got := <-receivedTerminalHeader; got != terminalHeader {
			t.Fatalf("provider X-Plan15-Terminal = %q, want %q", got, terminalHeader)
		}
	})

	executor, err := plugin.NewBufferedResponseExecutor(nil,
		plugin.TerminalDescriptor{Owner: plugin.TerminalOwnerOrdinaryProxy},
		base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes})
	if err != nil {
		t.Fatalf("NewBufferedResponseExecutor() error = %v", err)
	}
	pipeline := plugin.NewRequestPipeline(nil, nil).WithBufferedResponseExecutor(executor)
	called := false
	handler := pipeline.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		if _, ok := w.(http.Flusher); !ok {
			t.Error("transparent writer lost http.Flusher")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("transparent writer lost http.Hijacker")
		}
		if _, ok := w.(http.Pusher); !ok {
			t.Error("transparent writer lost http.Pusher")
		}
		if _, ok := w.(io.ReaderFrom); !ok {
			t.Error("transparent writer lost io.ReaderFrom")
		}
	}))
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(0, 0),
	)
	writer := &routeOptionalWriter{header: make(http.Header)}
	handler.ServeHTTP(writer, request)
	if !called {
		t.Fatal("terminal was not called")
	}
}

func TestNoBoundedPlanPreservesFirstTerminalOwnerWhenMultipleAreEnabled(t *testing.T) {
	ensureRouteStore(t)
	route := resource.Route{
		ID: "multiple-terminal-no-bounded-route",
		Plugins: map[string]resource.PluginConfig{
			"ai-proxy": map[string]any{
				"provider": "openai-compatible",
				"auth":     map[string]any{},
				"override": map[string]any{"endpoint": "http://provider.test"},
			},
			"dubbo-proxy": map[string]any{
				"service_name":    "org.example.EchoService",
				"service_version": "1.0.0",
			},
		},
	}
	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	if _, err := builder.buildHandlerStrict(route); err != nil {
		t.Fatalf("no-bounded multiple-terminal route build error = %v", err)
	}

	descriptor, err := terminalDescriptorForRoute(
		materializedPluginSources(
			route.Plugins,
			plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID},
		),
		resource.Upstream{},
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID},
	)
	if err != nil {
		t.Fatalf("terminalDescriptorForRoute() error = %v", err)
	}
	if descriptor != (plugin.TerminalDescriptor{
		Owner: plugin.TerminalOwnerAIRuntime,
		Provenance: plugin.ResourceProvenance{
			Kind: plugin.ResourceRoute,
			ID:   route.ID,
		},
	}) {
		t.Fatalf("terminal descriptor = %#v, want deterministic first AI route owner", descriptor)
	}

	boundedPlugin := &routeBufferedPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
	}
	boundedBinding := checkedRouteBinding(t, "echo", boundedPlugin, plugin.ScopeRoute)
	if _, err := plugin.NewBufferedResponseExecutor(
		[]plugin.Binding{boundedBinding},
		descriptor,
		base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	); err == nil || !strings.Contains(err.Error(), "conflicts with terminal owner") {
		t.Fatalf("static bounded multiple-terminal error = %v, want terminal conflict", err)
	}

	dynamicBinding := checkedRouteBinding(t, "echo", boundedPlugin, plugin.ScopeConsumer)
	executor, err := plugin.NewBufferedResponseExecutor(
		nil,
		descriptor,
		base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	)
	if err != nil {
		t.Fatalf("transparent NewBufferedResponseExecutor() error = %v", err)
	}
	terminalCalls := 0
	handler := plugin.NewRequestPipeline(nil, func(r *http.Request) (plugin.ConsumerResolution, error) {
		return plugin.ConsumerResolution{
			Request:  r,
			Resolved: true,
			Bindings: []plugin.Binding{dynamicBinding},
		}, nil
	}).WithBufferedResponseExecutor(executor).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		terminalCalls++
	}))
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(0, 0),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if terminalCalls != 0 || response.Code != http.StatusInternalServerError {
		t.Fatalf(
			"dynamic terminal/status = %d/%d, want conflict before terminal and stable 500",
			terminalCalls,
			response.Code,
		)
	}
}

func TestTerminalOwnerIgnoresDisabledBoundedTerminalPlugins(t *testing.T) {
	bounded := &routeBufferedPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
	}
	boundedBinding := checkedRouteBinding(t, "echo", bounded, plugin.ScopeRoute)
	for _, name := range []string{"ai-proxy", "ai-proxy-multi", "dubbo-proxy", "http-dubbo"} {
		t.Run(name, func(t *testing.T) {
			ensureRouteStore(t)
			route := resource.Route{
				ID: "disabled-" + name,
				Plugins: map[string]resource.PluginConfig{
					"body-transformer": map[string]any{
						"response": map[string]any{"template": "ok"},
					},
					name: map[string]any{"_meta": map[string]any{"disable": true}},
				},
			}
			builder := NewBuilder(nil)
			t.Cleanup(builder.Stop)
			if _, err := builder.buildHandlerStrict(route); err != nil {
				t.Fatalf("disabled %s bounded route build error = %v", name, err)
			}
			sources := materializedPluginSources(
				route.Plugins,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID},
			)
			descriptor, err := terminalDescriptorForRoute(
				sources,
				resource.Upstream{},
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID},
			)
			if err != nil {
				t.Fatalf("terminalDescriptorForRoute() error = %v", err)
			}
			if descriptor.Owner != plugin.TerminalOwnerOrdinaryProxy {
				t.Fatalf("terminal owner = %d, want ordinary proxy", descriptor.Owner)
			}
			if _, err := plugin.NewBufferedResponseExecutor(
				[]plugin.Binding{boundedBinding},
				descriptor,
				base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
			); err != nil {
				t.Fatalf("disabled %s rejected bounded route: %v", name, err)
			}
		})
	}
}

func TestTerminalOwnerUsesPluginConfigWinnerProvenance(t *testing.T) {
	route := resource.Route{ID: "winner-route", PluginConfigID: "winner-config"}
	pluginConfig := map[string]resource.PluginConfig{
		"ai-proxy": map[string]any{},
	}
	service := resource.Service{
		ID: "shadowed-service",
		Plugins: map[string]resource.PluginConfig{
			"ai-proxy": map[string]any{},
		},
	}
	localSources, serviceSources, _ := selectMaterializedPluginSources(
		route.Plugins,
		route.ID,
		pluginConfig,
		route.PluginConfigID,
		service.Plugins,
		service.ID,
	)
	sources := append(localSources, serviceSources...)
	descriptor, err := terminalDescriptorForRoute(
		sources,
		resource.Upstream{},
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID},
	)
	if err != nil {
		t.Fatalf("terminalDescriptorForRoute() error = %v", err)
	}
	if descriptor.Owner != plugin.TerminalOwnerAIRuntime {
		t.Fatalf("terminal owner = %d, want AI runtime", descriptor.Owner)
	}
	if descriptor.Provenance != (plugin.ResourceProvenance{
		Kind: plugin.ResourcePluginConfig,
		ID:   route.PluginConfigID,
	}) {
		t.Fatalf("terminal provenance = %#v, want plugin-config winner", descriptor.Provenance)
	}
}

func TestTerminalHandlerDoesNotDefaultUnknownToUpstream(t *testing.T) {
	pluginInstance := &routeBufferedPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
		bodySuffix: "-must-not-run",
	}
	binding := checkedRouteBinding(t, "echo", pluginInstance, plugin.ScopeRoute)
	response := serveBufferedRoute(t, []plugin.Binding{binding}, nil,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("unknown"))
		}))
	if response.Code != http.StatusInternalServerError || pluginInstance.bodyCalls != 0 {
		t.Fatalf(
			"unknown-source response/body-callbacks = %d/%d, want stable 500/0",
			response.Code,
			pluginInstance.bodyCalls,
		)
	}
}

type routeCountingCommitter struct {
	calls  int
	events *[]string
}

func (c *routeCountingCommitter) CommitFinalResponse(
	w http.ResponseWriter,
	_ *http.Request,
	state *base.ResponseState,
	baseCommit plugin.BaseCommit,
) {
	c.calls++
	if c.events != nil {
		*c.events = append(*c.events, "commit")
	}
	baseCommit(w, state)
}

func TestBufferedRouteFinalCommitterPreserves103AndRunsBaseCommitOnce(t *testing.T) {
	pluginInstance := &routeBufferedPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
	}
	binding := checkedRouteBinding(t, "echo", pluginInstance, plugin.ScopeRoute)
	executor, err := plugin.NewBufferedResponseExecutor([]plugin.Binding{binding},
		plugin.TerminalDescriptor{Owner: plugin.TerminalOwnerOrdinaryProxy},
		base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes})
	if err != nil {
		t.Fatalf("NewBufferedResponseExecutor() error = %v", err)
	}
	committer := &routeCountingCommitter{}
	executor = executor.WithFinalResponseCommitter(committer)
	handler := plugin.NewRequestPipeline([]plugin.Binding{binding}, nil).
		WithBufferedResponseExecutor(executor).
		Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			w.WriteHeader(http.StatusEarlyHints)
			_, _ = w.Write([]byte("final"))
		}))
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(0, 0),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if committer.calls != 1 || response.Code != http.StatusEarlyHints ||
		response.Body.String() != "final" {
		t.Fatalf(
			"committer/status/body = %d/%d/%q, want 1/200/final",
			committer.calls,
			response.Code,
			response.Body.String(),
		)
	}
}

func TestBufferedRouteFinalizerRunsAfterOutcomeBeforeRecycle(t *testing.T) {
	events := make([]string, 0, 6)
	pluginInstance := &routeBufferedPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
		events:     &events,
	}
	storePlugin := &routeBufferedPlugin{name: "proxy-cache", events: &events}
	binding := checkedRouteBinding(t, "echo", pluginInstance, plugin.ScopeRoute)
	storeBinding := checkedRouteBinding(t, "proxy-cache", storePlugin, plugin.ScopeRoute)
	executor, err := plugin.NewBufferedResponseExecutor(
		[]plugin.Binding{binding, storeBinding},
		plugin.TerminalDescriptor{Owner: plugin.TerminalOwnerOrdinaryProxy},
		base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	)
	if err != nil {
		t.Fatalf("NewBufferedResponseExecutor() error = %v", err)
	}
	executor = executor.WithFinalResponseCommitter(&routeCountingCommitter{events: &events})
	handler := plugin.NewRequestPipeline([]plugin.Binding{binding, storeBinding}, nil).
		WithBufferedResponseExecutor(executor).
		Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			events = append(events, "response execution")
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			_, _ = w.Write([]byte("origin"))
		}))
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(0, 0),
	)
	lifecycle.AddFinalizer("route-test", func() error {
		events = append(events, "finalizer")
		return nil
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	lifecycle.SetOutcome(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Committed: true},
	)
	events = append(events, "outcome")
	if lifecycle.Outcome().Kind != apisixctx.RequestOutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed before finalizer", lifecycle.Outcome().Kind)
	}
	lifecycle.Finalize()
	events = append(events, "recycle")
	apisixctx.RecycleVars(request)
	if got, want := strings.Join(events, ","),
		"response execution,body,store,commit,outcome,finalizer,recycle"; got != want {
		t.Fatalf("lifecycle events = %q, want %q", got, want)
	}
}
