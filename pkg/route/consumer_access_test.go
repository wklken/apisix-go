package route

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestConsumerResolutionMissingGroupFailsClosed(t *testing.T) {
	_, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Consumers: map[string]resource.Consumer{
			"missing-group-consumer": {
				Username: "missing-group-consumer",
				GroupID:  "missing-group",
			},
		},
	})
	if err == nil {
		t.Fatal("PlanHTTPPlugins() error = nil, want missing-group failure")
	}
	if !strings.Contains(err.Error(), "missing-group") {
		t.Fatalf("PlanHTTPPlugins() error = %q, want group provenance", err)
	}
}

func TestConsumerResolutionPreservesGroupAndConsumerProvenance(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		EnabledPlugins: []string{"request-id", "proxy-rewrite", "jwt-auth", "jwe-decrypt", "basic-auth"},
		Consumers: map[string]resource.Consumer{
			"provenance-consumer": {
				Username: "provenance-consumer",
				GroupID:  "provenance-group",
				Plugins: map[string]resource.PluginConfig{
					"proxy-rewrite": map[string]any{
						"headers": map[string]any{"set": map[string]any{"X-Provenance": "consumer"}},
					},
					"basic-auth": map[string]any{"username": "ignored", "password": "ignored"},
				},
			},
		},
		ConsumerGroups: map[string]resource.ConsumerGroup{
			"provenance-group": {
				Plugins: map[string]resource.PluginConfig{
					"request-id": map[string]any{},
					"proxy-rewrite": map[string]any{
						"headers": map[string]any{"set": map[string]any{"X-Provenance": "group"}},
					},
					"jwt-auth":    map[string]any{"key": "group-key"},
					"jwe-decrypt": map[string]any{"key": "group-jwe"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	plans := plan.Consumers["provenance-consumer"]
	if len(plans) != 2 {
		t.Fatalf("consumer plans = %d, want group and consumer winners", len(plans))
	}

	provenance := make(map[string]plugin.ResourceProvenance, len(plans))
	for _, consumerPlan := range plans {
		provenance[consumerPlan.Factory] = consumerPlan.Provenance
		if consumerPlan.Scope != plugin.ScopeConsumer {
			t.Fatalf("plan %q scope = %d, want consumer scope", consumerPlan.Factory, consumerPlan.Scope)
		}
	}
	if got, want := provenance["request-id"], (plugin.ResourceProvenance{Kind: plugin.ResourceConsumerGroup, ID: "provenance-group"}); got != want {
		t.Fatalf("group provenance = %#v, want %#v", got, want)
	}
	if got, want := provenance["proxy-rewrite"], (plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "provenance-consumer"}); got != want {
		t.Fatalf("consumer provenance = %#v, want %#v", got, want)
	}
	if _, ok := provenance["jwt-auth"]; ok {
		t.Fatal("credential-only jwt-auth plan returned")
	}
	if _, ok := provenance["jwe-decrypt"]; ok {
		t.Fatal("credential-only jwe-decrypt plan returned")
	}
}

func TestAuthenticatedRouteOverwritesForgedConsumerHeader(t *testing.T) {
	tests := []struct {
		name       string
		consumerID string
		pluginName string
		consumer   resource.Consumer
		setAuth    func(*http.Request)
	}{
		{
			name:       "basic-auth",
			consumerID: "route-basic-header",
			pluginName: "basic-auth",
			consumer: resource.Consumer{
				Username: "route-basic-header",
				Plugins: map[string]resource.PluginConfig{
					"basic-auth": map[string]any{
						"username": "route-basic-header",
						"password": "route-basic-secret",
					},
				},
			},
			setAuth: func(request *http.Request) {
				encoded := base64.StdEncoding.EncodeToString([]byte("route-basic-header:route-basic-secret"))
				request.Header.Set("Authorization", "Basic "+encoded)
			},
		},
		{
			name:       "key-auth",
			consumerID: "route-key-header",
			pluginName: "key-auth",
			consumer: resource.Consumer{
				Username: "route-key-header",
				Plugins: map[string]resource.PluginConfig{
					"key-auth": map[string]any{"key": "route-key-secret"},
				},
			},
			setAuth: func(request *http.Request) {
				request.Header.Set("apikey", "route-key-secret")
			},
		},
		{
			name:       "jwt-auth",
			consumerID: "route-jwt-header",
			pluginName: "jwt-auth",
			consumer: resource.Consumer{
				Username: "route-jwt-header",
				Plugins: map[string]resource.PluginConfig{
					"jwt-auth": map[string]any{
						"key":       "route-jwt-key",
						"secret":    "route-jwt-secret",
						"algorithm": "HS256",
					},
				},
			},
			setAuth: func(request *http.Request) {
				request.Header.Set("Authorization", "Bearer "+signedScopedJWT(t, "route-jwt-key", "route-jwt-secret"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			seen := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Get("X-Consumer-Username")
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

			route := resource.Route{
				ID:  "route-header-" + test.name,
				Uri: "/route-header-" + test.name,
				Upstream: resource.Upstream{
					Type:   "roundrobin",
					Scheme: upstreamURL.Scheme,
					Nodes:  []resource.Node{{Host: upstreamURL.Hostname(), Port: port, Weight: 1}},
				},
			}
			lookupKey := test.consumerID
			if test.pluginName == "key-auth" {
				lookupKey = "route-key-secret"
			}
			if test.pluginName == "jwt-auth" {
				lookupKey = "route-jwt-key"
			}
			lookup := &testConsumerLookup{byKey: map[string]resource.Consumer{lookupKey: test.consumer}}
			binding := testPluginBindingWithDependencies(t, test.pluginName, map[string]any{}, route, base.Dependencies{
				Consumers: lookup,
			})
			handler := testPreparedConsumerHandler(t, route, map[string]PreparedConsumerRecord{
				test.consumerID: {Consumer: test.consumer},
			}, []plugin.Binding{binding}, upstream.URL)

			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/route-header-"+test.name, nil)
			request.Header.Set("X-Consumer-Username", "attacker")
			test.setAuth(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
			}
			select {
			case got := <-seen:
				if got != test.consumerID {
					t.Fatalf("upstream consumer header = %q, want %q", got, test.consumerID)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for authenticated upstream request")
			}
		})
	}
}

func TestPlan14V2BeforeProxyAndFinalizeRunOnce(t *testing.T) {
	hookCalls := 0
	request := httptest.NewRequest(http.MethodGet, "/before", nil)
	request = apisixctx.WithBeforeProxyHook(request, func(*http.Request) error {
		hookCalls++
		return nil
	})
	pipeline := plugin.NewRequestPipeline(nil, func(r *http.Request) (plugin.ConsumerResolution, error) {
		return plugin.ConsumerResolution{Request: r, Resolved: true}, nil
	})
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/before" {
			t.Fatalf("terminal path = %q, want /before", r.URL.Path)
		}
	})).ServeHTTP(httptest.NewRecorder(), request)
	if hookCalls != 1 {
		t.Fatalf("before-proxy hooks = %d, want one pipeline owner", hookCalls)
	}
}

type testConsumerLookup struct {
	byKey map[string]resource.Consumer
}

func (lookup *testConsumerLookup) ConsumerByPluginKey(_, key string) (resource.Consumer, bool) {
	consumer, ok := lookup.byKey[key]
	return consumer, ok
}

func (lookup *testConsumerLookup) ConsumerByID(id string) (resource.Consumer, bool) {
	consumer, ok := lookup.byKey[id]
	return consumer, ok
}

func (*testConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func testPluginBindingWithDependencies(
	t testing.TB,
	name string,
	config resource.PluginConfig,
	routeResource resource.Route,
	deps base.Dependencies,
) plugin.Binding {
	return testPluginBindingWithDependenciesScope(
		t,
		name,
		config,
		routeResource,
		deps,
		plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID},
	)
}

func testPluginBindingWithDependenciesScope(
	t testing.TB,
	name string,
	config resource.PluginConfig,
	routeResource resource.Route,
	deps base.Dependencies,
	scope plugin.Scope,
	provenance plugin.ResourceProvenance,
) plugin.Binding {
	t.Helper()
	instance := plugin.New(name, deps)
	if instance == nil {
		t.Fatalf("plugin %q is not supported", name)
	}
	if err := instance.Init(); err != nil {
		t.Fatalf("plugin %q Init() error = %v", name, err)
	}
	if err := util.Parse(config, instance.Config()); err != nil {
		t.Fatalf("plugin %q config error = %v", name, err)
	}
	if setter, ok := instance.(interface{ SetRouteContext(string, string) }); ok {
		setter.SetRouteContext(routeResource.ID, "127.0.0.1:9080")
	}
	if setter, ok := instance.(interface {
		SetResourceContext(resource.Route, resource.Service)
	}); ok {
		setter.SetResourceContext(routeResource, resource.Service{})
	}
	if materializer, ok := instance.(interface {
		MaterializeScopedSecrets(context.Context, base.ScopedSecretAccess) error
	}); ok {
		if err := materializer.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
			t.Fatalf("plugin %q secret preparation error = %v", name, err)
		}
	}
	if err := instance.PostInit(); err != nil {
		t.Fatalf("plugin %q PostInit() error = %v", name, err)
	}
	if stopper, ok := instance.(interface{ Stop() }); ok {
		t.Cleanup(stopper.Stop)
	}
	binding, err := plugin.BindPluginChecked(
		name,
		instance,
		scope,
		provenance,
	)
	if err != nil {
		t.Fatalf("BindPluginChecked(%q) error = %v", name, err)
	}
	return binding
}

func testPreparedConsumerHandler(
	t testing.TB,
	routeResource resource.Route,
	consumers map[string]PreparedConsumerRecord,
	staticBindings []plugin.Binding,
	upstreamURL string,
) http.Handler {
	t.Helper()
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse prepared upstream URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse prepared upstream port: %v", err)
	}
	routeResource.Upstream = resource.Upstream{
		Type: "roundrobin", Scheme: parsed.Scheme,
		Nodes: []resource.Node{{Host: parsed.Hostname(), Port: port, Weight: 1}},
	}
	effective := testEffectiveConfig()
	plan, err := PlanRouteUpstream(routeResource, resource.Service{}, nil, nil, &effective.Config)
	if err != nil {
		t.Fatalf("PlanRouteUpstream() error = %v", err)
	}
	if plan.ClusterConfig == nil {
		t.Fatal("PlanRouteUpstream() cluster config = nil")
	}
	cluster, err := pxy.NewCluster(*plan.ClusterConfig, pxy.NopClusterObserver{})
	if err != nil {
		t.Fatalf("NewCluster() error = %v", err)
	}
	t.Cleanup(cluster.Close)
	handler, err := BuildPreparedHandler(PreparedHandlerInput{
		Route:          routeResource,
		Consumers:      consumers,
		StaticBindings: staticBindings,
		Upstream:       plan,
		Runtime: PreparedUpstreamRuntime{
			LoadBalancer: cluster.LoadBalancer(), RoundTripper: cluster.RoundTripper(),
		},
		StaticConfig: effective.Config,
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}
	return handler
}
