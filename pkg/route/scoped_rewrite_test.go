package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
	apisixruntime "github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

type scopedRewriteTestPlugin struct {
	name     string
	priority int
	order    *[]string
	path     string
	stop     bool
	status   int
}

func (p *scopedRewriteTestPlugin) Init() error               { return nil }
func (p *scopedRewriteTestPlugin) PostInit() error           { return nil }
func (p *scopedRewriteTestPlugin) Config() any               { return nil }
func (p *scopedRewriteTestPlugin) GetSchema() string         { return "" }
func (p *scopedRewriteTestPlugin) GetMetadataSchema() string { return "" }
func (p *scopedRewriteTestPlugin) GetPriority() int          { return p.priority }
func (p *scopedRewriteTestPlugin) GetName() string           { return p.name }
func (p *scopedRewriteTestPlugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*p.order = append(*p.order, p.name+":handler")
		next.ServeHTTP(w, r)
	})
}

func (p *scopedRewriteTestPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	*p.order = append(*p.order, p.name)
	if p.path != "" {
		r = r.Clone(r.Context())
		r.URL.Path = p.path
	}
	if p.status != 0 {
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte("original"))
	}
	if p.stop {
		w.WriteHeader(http.StatusTeapot)
		return base.StopRequest(r)
	}
	return base.ContinueRequest(r)
}

var (
	_ plugin.Plugin           = (*scopedRewriteTestPlugin)(nil)
	_ base.RequestPhasePlugin = (*scopedRewriteTestPlugin)(nil)
)

func bindScopedTestPlugin(
	factory string,
	p plugin.Plugin,
	scope plugin.Scope,
	provenance plugin.ResourceProvenance,
) plugin.Binding {
	binding := bindPluginForTest(factory, p, scope, provenance)
	binding.Priority = p.GetPriority()
	return binding
}

func TestScopedRewriteRunsSystemThenGlobalThenRoute(t *testing.T) {
	order := []string{}
	executor := assembleRouteExecutor(
		[]plugin.Binding{
			bindScopedTestPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "route", priority: 10000, order: &order},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-1"},
			),
		},
		[]plugin.Binding{
			bindScopedTestPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "global", priority: 1, order: &order},
				plugin.ScopeGlobal,
				plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global-1"},
			),
		},
		[]plugin.Binding{
			bindScopedTestPlugin(
				"example-plugin",
				&scopedRewriteTestPlugin{name: "system", priority: 1, order: &order},
				plugin.ScopeSystem,
				plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "example-plugin"},
			),
		},
	)
	response := httptest.NewRecorder()
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		order = append(order, "upstream")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/scoped", nil))

	if got, want := strings.Join(order, ","), "system,global,route,upstream"; got != want {
		t.Fatalf("execution order = %q, want %q", got, want)
	}
}

func TestScopedRewriteUsesPriorityOnlyWithinScope(t *testing.T) {
	order := []string{}
	executor := assembleRouteExecutor(
		[]plugin.Binding{
			bindScopedTestPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "route-low", priority: 1, order: &order},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-low"},
			),
			bindScopedTestPlugin(
				"proxy-rewrite",
				&scopedRewriteTestPlugin{name: "route-high", priority: 100, order: &order},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-high"},
			),
		},
		[]plugin.Binding{
			bindScopedTestPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "global-low", priority: -100, order: &order},
				plugin.ScopeGlobal,
				plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global-low"},
			),
		},
		nil,
	)
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		order = append(order, "upstream")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/scoped", nil))

	if got, want := strings.Join(order, ","), "global-low,route-high,route-low,upstream"; got != want {
		t.Fatalf("execution order = %q, want %q", got, want)
	}
}

func TestScopedRewritePreservesServiceAndPluginConfigProvenance(t *testing.T) {
	provenance := []plugin.ResourceProvenance{
		{Kind: plugin.ResourceService, ID: "service-1"},
		{Kind: plugin.ResourcePluginConfig, ID: "plugin-config-1"},
		{Kind: plugin.ResourceRoute, ID: "route-1"},
	}
	routeSources, serviceSources, _ := selectMaterializedPluginSources(
		map[string]resource.PluginConfig{"request-id": map[string]any{"header_name": "X-Route"}},
		"route-1",
		map[string]resource.PluginConfig{
			"request-id": map[string]any{"header_name": "X-Plugin-Config"},
			"real-ip":    map[string]any{},
		},
		"plugin-config-1",
		map[string]resource.PluginConfig{
			"request-id":    map[string]any{"header_name": "X-Service"},
			"traffic-label": map[string]any{},
		},
		"service-1",
	)
	got := make(map[string]plugin.ResourceProvenance)
	for _, source := range append(routeSources, serviceSources...) {
		got[source.name] = source.provenance
	}
	want := map[string]plugin.ResourceProvenance{
		"request-id":    provenance[2],
		"real-ip":       provenance[1],
		"traffic-label": provenance[0],
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("materialized provenance = %#v, want %#v", got, want)
	}
}

func TestScopedRewriteMaterializesRoutePluginConfigAndServiceWinners(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	node := upstreamNode(t, upstream.URL)
	routes := []resource.Route{
		{
			ID: "scoped-source-service-route", Uri: "/scoped-source-service",
			ServiceID: "scoped-source-service",
			Upstream: resource.Upstream{
				Type:   "roundrobin",
				Scheme: "http",
				Nodes:  []resource.Node{node},
			},
		},
		{
			ID: "scoped-source-pc-route", Uri: "/scoped-source-pc",
			PluginConfigID: "scoped-source-pc", ServiceID: "scoped-source-service",
			Upstream: resource.Upstream{
				Type:   "roundrobin",
				Scheme: "http",
				Nodes:  []resource.Node{node},
			},
		},
		{
			ID: "scoped-source-route-route", Uri: "/scoped-source-route",
			PluginConfigID: "scoped-source-pc", ServiceID: "scoped-source-service",
			Plugins: map[string]resource.PluginConfig{
				"request-id": map[string]any{"header_name": "X-Route"},
			},
			Upstream: resource.Upstream{
				Type:   "roundrobin",
				Scheme: "http",
				Nodes:  []resource.Node{node},
			},
		},
	}
	services := map[string]resource.Service{"scoped-source-service": {
		ID: "scoped-source-service",
		Plugins: map[string]resource.PluginConfig{
			"request-id": map[string]any{"header_name": "X-Service"},
		},
	}}
	pluginConfigs := map[string]resource.PluginConfigRule{"scoped-source-pc": {
		Plugins: map[string]resource.PluginConfig{
			"request-id": map[string]any{"header_name": "X-Plugin-Config"},
		},
	}}
	effective := httpPluginAllowlist("request-id")
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes: routes, Services: services, PluginConfigs: pluginConfigs,
		EnabledPlugins: []string{"request-id"},
	})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	prepared := make([]PreparedRoute, 0, len(plan.Routes))
	for _, planned := range plan.Routes {
		plans := append(append([]PluginPlan(nil), planned.ServicePlans...), planned.Local...)
		bindings := make([]plugin.Binding, 0, len(plans))
		for _, pluginPlan := range plans {
			binding := testPluginBindingForSource(
				t, pluginPlan.Factory, pluginPlan.Config, pluginPlan.Scope, pluginPlan.Provenance,
				planned.Route, planned.Service, "127.0.0.1:9080",
			)
			binding, err = pluginPlan.Apply(binding)
			if err != nil {
				t.Fatalf("PluginPlan.Apply(%s) error = %v", pluginPlan.Factory, err)
			}
			bindings = append(bindings, binding)
		}
		handler := testPreparedProxyHandler(
			t,
			planned.Route,
			planned.Service,
			effective,
			bindings...)
		prepared = append(prepared, PreparedRoute{
			Route: planned.Route, Hosts: planned.Route.EffectiveHosts(), Handler: handler,
		})
	}
	snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: prepared})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}
	handler := snapshot.Handler()
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/scoped-source-service", want: "X-Service"},
		{path: "/scoped-source-pc", want: "X-Plugin-Config"},
		{path: "/scoped-source-route", want: "X-Route"},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := performRouteTestRequest(t, handler, test.path)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", response.Code)
			}
			if got := response.Header().Get(test.want); got == "" {
				t.Fatalf("winner header %s = %q, want request id", test.want, got)
			}
			for _, other := range []string{"X-Service", "X-Plugin-Config", "X-Route"} {
				if other != test.want && response.Header().Get(other) != "" {
					t.Fatalf(
						"overridden header %s = %q, want empty",
						other,
						response.Header().Get(other),
					)
				}
			}
		})
	}
}

func TestScopedRewriteFilterAndErrorResponse(t *testing.T) {
	order := []string{}
	filter, err := compileScopedRewriteFilter()
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	phase := &scopedRewriteTestPlugin{name: "filtered", priority: 1, order: &order}
	wrapped := metadataRequestPlugin{
		Plugin:        phase,
		phase:         phase,
		filter:        filter,
		errorResponse: map[string]any{"message": "custom"},
	}
	executor := assembleRouteExecutor(
		[]plugin.Binding{
			bindScopedTestPlugin(
				"request-id",
				wrapped,
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-meta"},
			),
		},
		nil,
		nil,
	)
	response := httptest.NewRecorder()
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/meta?enabled=no", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("filtered status = %d, want 204", response.Code)
	}

	order = nil
	phase.stop = true
	phase.status = http.StatusUnauthorized
	response = httptest.NewRecorder()
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/meta?enabled=yes", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("error-response status = %d, want 401", response.Code)
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"message":"custom"}` {
		t.Fatalf("error-response body = %q, want custom body", got)
	}
}

func TestScopedRewriteEarlyStopSkipsLegacyAndUpstream(t *testing.T) {
	order := []string{}
	executor := assembleRouteExecutor(
		[]plugin.Binding{
			bindScopedTestPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "stop", priority: 1, order: &order, stop: true},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-stop"},
			),
			bindScopedTestPlugin(
				"response-rewrite",
				&recordingPlugin{name: "legacy", priority: 200, order: &order},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-legacy"},
			),
		},
		nil,
		nil,
	)
	response := httptest.NewRecorder()
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		order = append(order, "upstream")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stop", nil))
	if response.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", response.Code)
	}
	if !reflect.DeepEqual(order, []string{"legacy", "stop"}) {
		t.Fatalf("execution order = %#v, want [legacy stop]", order)
	}
}

func TestScopedRewriteGlobalNotFoundRunsSystemAndGlobalOnly(t *testing.T) {
	order := []string{}
	handler := assembleRouteExecutor(
		nil,
		[]plugin.Binding{
			bindScopedTestPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "global", priority: 1, order: &order},
				plugin.ScopeGlobal,
				plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global-404"},
			),
		},
		[]plugin.Binding{
			bindScopedTestPlugin(
				"example-plugin",
				&scopedRewriteTestPlugin{name: "system", priority: 1, order: &order},
				plugin.ScopeSystem,
				plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "example-plugin"},
			),
		},
	).Then(http.NotFoundHandler())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if got, want := strings.Join(order, ","), "system,global"; got != want {
		t.Fatalf("execution order = %q, want %q", got, want)
	}
}

func TestGlobalNotFoundInitializesCoreAPISIXVarsWithoutSystemPlugin(t *testing.T) {
	effective := testEffectiveConfig()
	effective.Config.Apisix.ID = "node-1"
	effective.Config.Plugins = nil
	effective.Config.NginxConfig.HTTP.ClientMaxBodySize = 1
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	if len(plan.System) != 0 {
		t.Fatalf("system plans = %#v, want none", plan.System)
	}
	handler, err := BuildPreparedNotFoundHandler("node-1", nil)
	if err != nil {
		t.Fatalf("BuildPreparedNotFoundHandler() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/missing", nil),
		time.Time{},
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	final := lifecycle.FinalRequest()
	if final == nil {
		t.Fatal("final request = nil, want lifecycle final request")
	}
	if vars := apisixctx.GetApisixVars(final); vars == nil {
		t.Fatal("global not-found APISIX vars = nil, want core vars")
	} else if vars["$node_id"] != "node-1" || vars["$route_id"] != "" {
		t.Fatalf("global not-found APISIX vars = %#v, want node and empty route IDs", vars)
	}
}

func TestPlan14V2JWTAuthPayloadFeedsEffectiveProxyRewrite(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-JWT-Payload")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(upstreamURL.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	routeResource := resource.Route{
		ID: "scoped-jwt-auth-route",
		Plugins: map[string]resource.PluginConfig{
			"jwt-auth": map[string]any{"store_in_ctx": true},
			"proxy-rewrite": map[string]any{
				"headers": map[string]any{
					"set": map[string]any{"X-JWT-Payload": "$jwt_auth_payload"},
				},
			},
		},
		Upstream: resource.Upstream{
			Type:   "roundrobin",
			Scheme: upstreamURL.Scheme,
			Nodes:  []resource.Node{{Host: upstreamURL.Hostname(), Port: port, Weight: 1}},
		},
	}
	consumer := resource.Consumer{
		Username: "scoped-jwt-auth-only",
		Plugins: map[string]resource.PluginConfig{"jwt-auth": map[string]any{
			"key": "user-key", "secret": "my-secret-key", "algorithm": "HS256",
		}},
	}
	consumerIndex, err := apisixruntime.NewConsumerBindings(
		[]apisixruntime.ConsumerRecord{{ID: consumer.Username, Consumer: consumer}},
		nil,
		[]apisixruntime.ConsumerCredentialBinding{
			{Plugin: "jwt-auth", Key: "user-key", ConsumerID: consumer.Username},
		},
	)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}
	t.Cleanup(consumerIndex.Close)
	auth := testPluginBindingForSourceWithDependencies(
		t, "jwt-auth", routeResource.Plugins["jwt-auth"], plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID},
		routeResource, resource.Service{}, "127.0.0.1:9080",
		base.Dependencies{Config: testEffectiveConfig(), Consumers: consumerIndex},
	)
	rewrite := testPluginBinding(
		t,
		"proxy-rewrite",
		routeResource.Plugins["proxy-rewrite"],
		routeResource,
	)
	handler := testPreparedProxyHandlerWithConsumers(
		t, routeResource, resource.Service{}, testEffectiveConfig(),
		map[string]PreparedConsumerRecord{consumer.Username: {Consumer: consumer}},
		auth, rewrite,
	)

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/scoped-jwt-auth", nil)
	request.Header.Set("Authorization", "Bearer "+signedScopedJWT(t, "user-key", "my-secret-key"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	select {
	case payload := <-seen:
		if !strings.Contains(payload, "key:user-key") {
			t.Fatalf("X-JWT-Payload = %q, want authenticated payload", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
}

func TestPlan14V2ConsumerProxyRewriteOverridesRouteBeforeEitherExecutes(t *testing.T) {
	seen := make(chan []string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Values("X-Consumer-Proxy")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(upstreamURL.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	routeResource := resource.Route{
		ID: "scoped-jwt-proxy-route",
		Plugins: map[string]resource.PluginConfig{
			"jwt-auth": map[string]any{},
			"proxy-rewrite": map[string]any{
				"headers": map[string]any{
					"add": map[string]any{"X-Consumer-Proxy": "route"},
				},
			},
		},
		Upstream: resource.Upstream{
			Type:   "roundrobin",
			Scheme: upstreamURL.Scheme,
			Nodes:  []resource.Node{{Host: upstreamURL.Hostname(), Port: port, Weight: 1}},
		},
	}
	consumer := resource.Consumer{
		Username: "scoped-jwt-proxy-consumer",
		Plugins: map[string]resource.PluginConfig{
			"jwt-auth": map[string]any{
				"key": "consumer-key", "secret": "consumer-secret", "algorithm": "HS256",
			},
			"proxy-rewrite": map[string]any{
				"headers": map[string]any{"add": map[string]any{"X-Consumer-Proxy": "consumer"}},
			},
		},
	}
	consumerIndex, err := apisixruntime.NewConsumerBindings(
		[]apisixruntime.ConsumerRecord{{ID: consumer.Username, Consumer: consumer}},
		nil,
		[]apisixruntime.ConsumerCredentialBinding{
			{Plugin: "jwt-auth", Key: "consumer-key", ConsumerID: consumer.Username},
		},
	)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}
	t.Cleanup(consumerIndex.Close)
	auth := testPluginBindingForSourceWithDependencies(
		t, "jwt-auth", routeResource.Plugins["jwt-auth"], plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID},
		routeResource, resource.Service{}, "127.0.0.1:9080",
		base.Dependencies{Config: testEffectiveConfig(), Consumers: consumerIndex},
	)
	routeRewrite := testPluginBinding(
		t,
		"proxy-rewrite",
		routeResource.Plugins["proxy-rewrite"],
		routeResource,
	)
	consumerRewrite := testPluginBindingForSource(
		t, "proxy-rewrite", consumer.Plugins["proxy-rewrite"], plugin.ScopeConsumer,
		plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: consumer.Username},
		routeResource, resource.Service{}, "127.0.0.1:9080",
	)
	handler := testPreparedProxyHandlerWithConsumers(
		t,
		routeResource,
		resource.Service{},
		testEffectiveConfig(),
		map[string]PreparedConsumerRecord{
			consumer.Username: {Consumer: consumer, Bindings: []plugin.Binding{consumerRewrite}},
		},
		auth,
		routeRewrite,
	)

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/scoped-jwt-proxy", nil)
	request.Header.Set(
		"Authorization",
		"Bearer "+signedScopedJWT(t, "consumer-key", "consumer-secret"),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	select {
	case values := <-seen:
		if !reflect.DeepEqual(values, []string{"consumer"}) {
			t.Fatalf("X-Consumer-Proxy values = %#v, want one consumer value", values)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
}

func signedScopedJWT(t *testing.T, key, secret string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"key": key})
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signed
}

func TestServiceProvenanceUsesAuthoritativeRouteServiceID(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes: []resource.Route{{ID: "scoped-service-route", ServiceID: "scoped-service-key"}},
		Services: map[string]resource.Service{"scoped-service-key": {
			Plugins: map[string]resource.PluginConfig{
				"proxy-rewrite": map[string]any{"method": "INVALID"},
			},
		}},
		EnabledPlugins: []string{"proxy-rewrite"},
	})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	if len(plan.Routes) != 1 || len(plan.Routes[0].Local) != 1 {
		t.Fatalf("service plans = %#v, want one proxy-rewrite plan", plan.Routes)
	}
	servicePlan := plan.Routes[0].Local[0]
	if servicePlan.Provenance != (plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: "scoped-service-key"}) {
		t.Fatalf(
			"service provenance = %#v, want authoritative route service ID",
			servicePlan.Provenance,
		)
	}
	instance := plugin.New(servicePlan.Factory, base.Dependencies{Config: testEffectiveConfig()})
	if instance == nil {
		t.Fatal("proxy-rewrite plugin is unavailable")
	}
	if err := instance.Init(); err != nil {
		t.Fatalf("proxy-rewrite Init() error = %v", err)
	}
	if err := util.Parse(servicePlan.Config, instance.Config()); err != nil {
		t.Fatalf("proxy-rewrite config parse error = %v", err)
	}
	if err := instance.PostInit(); err == nil {
		t.Fatal("proxy-rewrite PostInit() error = nil, want invalid service plugin config")
	}
}

func TestBuildRejectsGlobalRuleWithoutEmbeddedID(t *testing.T) {
	_, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		GlobalRules: []resource.GlobalRule{{
			Plugins: map[string]resource.PluginConfig{"request-id": map[string]any{}},
		}},
		EnabledPlugins: []string{"request-id"},
	})
	if err == nil {
		t.Fatal("PlanHTTPPlugins() error = nil, want fail-closed missing global-rule ID")
	}
	if !strings.Contains(err.Error(), "global rule") ||
		!strings.Contains(err.Error(), "id is required") {
		t.Fatalf("PlanHTTPPlugins() error = %q, want missing global-rule ID diagnostic", err)
	}
}

func compileScopedRewriteFilter() (*pluginexpr.Expression, error) {
	return pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
}
