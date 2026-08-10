package route

import (
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
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
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

func TestScopedRewriteRunsSystemThenGlobalThenRoute(t *testing.T) {
	order := []string{}
	executor := assembleRouteExecutor(
		[]plugin.Binding{
			plugin.BindPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "route", priority: 10000, order: &order},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-1"},
			),
		},
		[]plugin.Binding{
			plugin.BindPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "global", priority: 1, order: &order},
				plugin.ScopeGlobal,
				plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global-1"},
			),
		},
		[]plugin.Binding{
			plugin.BindPlugin(
				"request-context",
				&scopedRewriteTestPlugin{name: "system", priority: 1, order: &order},
				plugin.ScopeSystem,
				plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "request-context"},
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
			plugin.BindPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "route-low", priority: 1, order: &order},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-low"},
			),
			plugin.BindPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "route-high", priority: 100, order: &order},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-high"},
			),
		},
		[]plugin.Binding{
			plugin.BindPlugin(
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
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t, "request-id")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	node := routePriorityNode(t, upstream.URL)
	putHTTPAllowlistResource(
		t,
		"plugin_configs",
		"scoped-source-pc",
		[]byte(`{"id":"scoped-source-pc","plugins":{"request-id":{"header_name":"X-Plugin-Config"}}}`),
	)
	putHTTPAllowlistResource(
		t,
		"services",
		"scoped-source-service",
		[]byte(`{"id":"scoped-source-service","plugins":{"request-id":{"header_name":"X-Service"}}}`),
	)
	putRouteResource(
		t,
		"scoped-source-service-route",
		[]byte(
			`{"id":"scoped-source-service-route","uri":"/scoped-source-service","service_id":"scoped-source-service","upstream":{"type":"roundrobin","nodes":{"`+node+`":1}}}`,
		),
	)
	putRouteResource(
		t,
		"scoped-source-pc-route",
		[]byte(
			`{"id":"scoped-source-pc-route","uri":"/scoped-source-pc","plugin_config_id":"scoped-source-pc","service_id":"scoped-source-service","upstream":{"type":"roundrobin","nodes":{"`+node+`":1}}}`,
		),
	)
	putRouteResource(
		t,
		"scoped-source-route-route",
		[]byte(
			`{"id":"scoped-source-route-route","uri":"/scoped-source-route","plugin_config_id":"scoped-source-pc","service_id":"scoped-source-service","plugins":{"request-id":{"header_name":"X-Route"}},"upstream":{"type":"roundrobin","nodes":{"`+node+`":1}}}`,
		),
	)

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil {
		t.Fatalf("BuildStrict() error = %v", err)
	}
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
					t.Fatalf("overridden header %s = %q, want empty", other, response.Header().Get(other))
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
			plugin.BindPlugin(
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
			plugin.BindPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "stop", priority: 1, order: &order, stop: true},
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-stop"},
			),
			plugin.BindPlugin(
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
			plugin.BindPlugin(
				"request-id",
				&scopedRewriteTestPlugin{name: "global", priority: 1, order: &order},
				plugin.ScopeGlobal,
				plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global-404"},
			),
		},
		[]plugin.Binding{
			plugin.BindPlugin(
				"request-context",
				&scopedRewriteTestPlugin{name: "system", priority: 1, order: &order},
				plugin.ScopeSystem,
				plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "request-context"},
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

func TestGlobalNotFoundInjectsOnlyRequestContextSystemPlugin(t *testing.T) {
	previousConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{
		Plugins:     nil,
		NginxConfig: appconfig.NginxConfig{HTTP: appconfig.NginxHTTP{ClientMaxBodySize: 1}},
	}
	t.Cleanup(func() { appconfig.GlobalConfig = previousConfig })
	builder := NewBuilder(nil)
	set := plugin.NewEnabledSet(nil)
	builder.enabledPlugins = &set
	handler, err := builder.buildGlobalNotFoundHandler(nil)
	if err != nil {
		t.Fatalf("buildGlobalNotFoundHandler() error = %v, want request-context-only system setup", err)
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
		t.Fatal("final request = nil, want request-context final request")
	}
	if vars := apisixctx.GetApisixVars(final); vars == nil {
		t.Fatal("global not-found APISIX vars = nil, want request-context system vars")
	} else if _, ok := vars["$route_id"]; !ok {
		t.Fatalf("global not-found APISIX vars = %#v, want $route_id from request-context", vars)
	}
}

func TestPlan14V2JWTAuthPayloadFeedsEffectiveProxyRewrite(t *testing.T) {
	ensureRouteStore(t)
	putHTTPAllowlistResource(t, "consumers", "scoped-jwt-auth-only", []byte(`{
		"username":"scoped-jwt-auth-only",
		"plugins":{"jwt-auth":{"key":"user-key","secret":"my-secret-key","algorithm":"HS256"}}
	}`))

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

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(resource.Route{
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
	})
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}

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
	ensureRouteStore(t)
	putHTTPAllowlistResource(t, "consumers", "scoped-jwt-proxy-consumer", []byte(`{
		"username":"scoped-jwt-proxy-consumer",
		"plugins":{
			"jwt-auth":{"key":"consumer-key","secret":"consumer-secret","algorithm":"HS256"},
			"proxy-rewrite":{"headers":{"add":{"X-Consumer-Proxy":"consumer"}}}
		}
	}`))

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

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(resource.Route{
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
	})
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/scoped-jwt-proxy", nil)
	request.Header.Set("Authorization", "Bearer "+signedScopedJWT(t, "consumer-key", "consumer-secret"))
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
	ensureRouteStore(t)
	putHTTPAllowlistResource(t, "services", "scoped-service-key", []byte(`{
		"plugins":{"proxy-rewrite":{"method":"INVALID"}}
	}`))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	_, err := builder.buildHandlerStrict(resource.Route{
		ID:        "scoped-service-route",
		ServiceID: "scoped-service-key",
	})
	if err == nil {
		t.Fatal("buildHandlerStrict() error = nil, want invalid service plugin config")
	}
	if !strings.Contains(err.Error(), `service "scoped-service-key"`) {
		t.Fatalf("buildHandlerStrict() error = %q, want authoritative service ID", err)
	}
}

func TestBuildRejectsGlobalRuleWithoutEmbeddedID(t *testing.T) {
	ensureRouteStore(t)
	putHTTPAllowlistResource(t, "global_rules", "scoped-global-missing-id", []byte(`{
		"plugins":{"request-id":{}}
	}`))
	putRouteResource(t, "scoped-global-missing-id-route", []byte(`{
		"id":"scoped-global-missing-id-route",
		"uri":"/scoped-global-missing-id",
		"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}
	}`))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("BuildStrict() = (%T, %v), want fail-closed missing global-rule ID", handler, err)
	}
	if !strings.Contains(err.Error(), "global rule ID") {
		t.Fatalf("BuildStrict() error = %q, want global rule ID diagnostic", err)
	}
}

func compileScopedRewriteFilter() (*pluginexpr.Expression, error) {
	return pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
}
