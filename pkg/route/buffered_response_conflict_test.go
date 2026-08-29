package route

import (
	"bufio"
	"bytes"
	"context"
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
	header  http.Header
	status  int
	body    bytes.Buffer
	flushed bool
}

type routeSuccessfulHijackWriter struct {
	routeOptionalWriter
	connection net.Conn
}

func (w *routeSuccessfulHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.connection, bufio.NewReadWriter(bufio.NewReader(w.connection), bufio.NewWriter(w.connection)), nil
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
func (w *routeOptionalWriter) Flush() { w.flushed = true }
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
		const terminalHeader = "reached-next"
		receivedTerminalHeader := make(chan string, 1)
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedTerminalHeader <- r.Header.Get("X-Plan15-Terminal")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[]}`))
		}))
		t.Cleanup(provider.Close)
		routeResource := resource.Route{
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
		}
		handler := testPreparedPluginHandler(
			t,
			routeResource,
			testPluginBinding(t, "ai-proxy", routeResource.Plugins["ai-proxy"], routeResource),
			testPluginBinding(t, "proxy-rewrite", routeResource.Plugins["proxy-rewrite"], routeResource),
		)
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

func TestAIRateLimitingSelectsBoundedOrStreamingResponsePlanPerRequest(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"total_tokens":1}}`))
	}))
	t.Cleanup(provider.Close)
	routeResource := resource.Route{
		ID: "dual-mode-ai-rate-route",
		Plugins: map[string]resource.PluginConfig{
			"ai-proxy": map[string]any{
				"provider": "openai-compatible",
				"auth":     map[string]any{},
				"override": map[string]any{"endpoint": provider.URL},
			},
			"ai-rate-limiting": map[string]any{"limit": 100, "time_window": 60},
		},
	}
	handler := testPreparedPluginHandler(
		t,
		routeResource,
		testPluginBinding(t, "ai-proxy", routeResource.Plugins["ai-proxy"], routeResource),
		testPluginBinding(t, "ai-rate-limiting", routeResource.Plugins["ai-rate-limiting"], routeResource),
	)
	for _, tc := range []struct {
		name      string
		body      string
		wantBody  string
		wantFlush bool
	}{
		{name: "bounded", body: `{"model":"test","messages":[]}`, wantBody: `{"choices":[],"usage":{"total_tokens":1}}`},
		{name: "streaming", body: `{"model":"test","messages":[],"stream":true}`, wantBody: "data: {\"choices\":[]}\n\n", wantFlush: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				"http://gateway.test/v1/chat/completions",
				strings.NewReader(tc.body),
			)
			request.Header.Set("Content-Type", "application/json")
			writer := &routeOptionalWriter{header: make(http.Header)}
			handler.ServeHTTP(writer, request)
			if writer.status != http.StatusOK || writer.body.String() != tc.wantBody || writer.flushed != tc.wantFlush {
				t.Fatalf(
					"response = %d/%q flushed=%v, want 200/%q flushed=%v",
					writer.status,
					writer.body.String(),
					writer.flushed,
					tc.wantBody,
					tc.wantFlush,
				)
			}
		})
	}
}

func TestAIContentModerationSelectsBoundedOrStreamingResponsePlanPerRequest(t *testing.T) {
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
	}))
	t.Cleanup(moderation.Close)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"safe\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"safe"}}]}`))
	}))
	t.Cleanup(provider.Close)
	for _, tc := range []struct {
		name            string
		streamCheckMode string
		requestBody     string
		wantFlush       bool
	}{
		{name: "bounded", streamCheckMode: "final_packet", requestBody: `{"model":"test","messages":[]}`},
		{name: "streaming", streamCheckMode: "realtime", requestBody: `{"model":"test","messages":[],"stream":true}`, wantFlush: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routeResource := resource.Route{ID: "dual-mode-ai-moderation-" + tc.name}
			handler := testPreparedPluginHandler(
				t,
				routeResource,
				testPluginBinding(t, "ai-proxy", map[string]any{
					"provider": "openai-compatible",
					"auth":     map[string]any{},
					"override": map[string]any{"endpoint": provider.URL},
				}, routeResource),
				testScopedSecretPluginBinding(t, "ai-aliyun-content-moderation", map[string]any{
					"endpoint": moderation.URL, "region_id": "cn-shanghai",
					"access_key_id": "key", "access_key_secret": "secret",
					"check_request": false, "check_response": true,
					"stream_check_mode": tc.streamCheckMode, "fail_mode": "warn",
				}, routeResource),
			)
			request := httptest.NewRequest(
				http.MethodPost,
				"http://gateway.test/v1/chat/completions",
				strings.NewReader(tc.requestBody),
			)
			request.Header.Set("Content-Type", "application/json")
			writer := &routeOptionalWriter{header: make(http.Header)}
			handler.ServeHTTP(writer, request)
			if writer.status != http.StatusOK || writer.flushed != tc.wantFlush ||
				!strings.Contains(writer.body.String(), "safe") {
				t.Fatalf(
					"response = %d/%q flushed=%v, want 200 safe flushed=%v",
					writer.status,
					writer.body.String(),
					writer.flushed,
					tc.wantFlush,
				)
			}
		})
	}
}

func TestResponsePlanRejectsMultipleEffectiveProtocolOwners(t *testing.T) {
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
	bindings := []plugin.Binding{
		testPluginBinding(t, "ai-proxy", route.Plugins["ai-proxy"], route),
		testPluginBinding(t, "dubbo-proxy", route.Plugins["dubbo-proxy"], route),
	}
	_, err := BuildPreparedHandler(PreparedHandlerInput{
		Route: route, StaticBindings: bindings,
		Runtime:      PreparedUpstreamRuntime{RoundTripper: http.DefaultTransport},
		StaticConfig: testEffectiveConfig().Config,
	})
	if err == nil || !strings.Contains(err.Error(), "ai-proxy") ||
		!strings.Contains(err.Error(), "dubbo-proxy") || !strings.Contains(err.Error(), route.ID) {
		t.Fatalf("multiple protocol owner error = %v, want both identities and route provenance", err)
	}
}

func TestTerminalOwnerIgnoresDisabledBoundedTerminalPlugins(t *testing.T) {
	for _, name := range []string{"ai-proxy", "ai-proxy-multi", "dubbo-proxy", "http-dubbo"} {
		t.Run(name, func(t *testing.T) {
			route := resource.Route{
				ID: "disabled-" + name,
				Plugins: map[string]resource.PluginConfig{
					"body-transformer": map[string]any{
						"response": map[string]any{"template": "ok"},
					},
					name: map[string]any{"_meta": map[string]any{"disable": true}},
				},
			}
			plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
				Routes: []resource.Route{route}, EnabledPlugins: []string{"body-transformer", name},
			})
			if err != nil {
				t.Fatalf("PlanHTTPPlugins() error = %v", err)
			}
			if len(plan.Routes) != 1 || len(plan.Routes[0].Local) != 1 ||
				plan.Routes[0].Local[0].Factory != "body-transformer" {
				t.Fatalf("disabled %s plan = %#v, want only body-transformer", name, plan.Routes)
			}
		})
	}
}

func TestTerminalOwnerUsesPluginConfigWinnerProvenance(t *testing.T) {
	route := resource.Route{ID: "winner-route", PluginConfigID: "winner-config"}
	pluginConfig := map[string]resource.PluginConfig{
		"dubbo-proxy": map[string]any{},
	}
	service := resource.Service{
		ID: "shadowed-service",
		Plugins: map[string]resource.PluginConfig{
			"dubbo-proxy": map[string]any{},
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
	bindings := make([]plugin.Binding, 0, len(sources))
	for _, source := range sources {
		bindings = append(bindings, plugin.Binding{
			Descriptor: plugin.Descriptor{Factory: source.name},
			Scope:      source.scope, Provenance: source.provenance,
		})
	}
	candidates := preparedRouteTerminalCandidates(
		bindings,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID},
		resource.Upstream{},
		routeProtocolTerminals{dubbo: routeDubboTerminal{}},
	)
	if len(candidates) != 1 || candidates[0].Identity != "dubbo-proxy" ||
		candidates[0].Provenance != (plugin.ResourceProvenance{
			Kind: plugin.ResourcePluginConfig,
			ID:   route.PluginConfigID,
		}) {
		t.Fatalf("route-owned candidates = %#v, want plugin-config winner", candidates)
	}
}

func TestRouteTerminalCandidatesUseResolvedKafkaUpstreamProvenance(t *testing.T) {
	provenance := plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: "kafka-upstream"}
	candidates := preparedRouteTerminalCandidates(
		nil,
		provenance,
		resource.Upstream{Scheme: "kafka"},
		routeProtocolTerminals{kafka: routeKafkaTerminal{handler: http.NotFoundHandler()}},
	)
	if len(candidates) != 1 || candidates[0].Identity != "kafka-proxy" ||
		candidates[0].Protocol != plugin.ProtocolKafka || candidates[0].Provenance != provenance {
		t.Fatalf("Kafka candidates = %#v, want resolved upstream provenance", candidates)
	}
}

func TestRouteKafkaTerminalReportsSuccessfulHijack(t *testing.T) {
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	t.Cleanup(func() { _ = peer.Close() })
	writer := &routeSuccessfulHijackWriter{
		routeOptionalWriter: routeOptionalWriter{header: make(http.Header)},
		connection:          server,
	}
	terminal := routeKafkaTerminal{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil || connection != server {
			t.Fatalf("Hijack() = %v/%v", connection, err)
		}
	})}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	disposition, _, source, err := terminal.RunExclusiveProtocol(writer, request, nil)
	if err != nil || disposition != base.ProtocolHijacked || source != apisixctx.ResponseSourceUpstream {
		t.Fatalf("Kafka terminal = disposition:%d source:%q err:%v", disposition, source, err)
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
