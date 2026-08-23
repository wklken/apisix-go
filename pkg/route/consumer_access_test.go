package route

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestConsumerResolutionMissingGroupFailsClosed(t *testing.T) {
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	resolver := builder.resolveConsumerBindings(builder.pluginRouteContext(resource.Route{ID: "missing-group-route"}))
	request := consumerResolutionRequest(resource.Consumer{
		Username: "missing-group-consumer",
		GroupID:  "missing-group",
	})

	resolution, err := resolver(request)
	if err == nil {
		t.Fatal("resolveConsumerBindings() error = nil, want missing-group failure")
	}
	if resolution.Request != request {
		t.Fatalf("failed resolution request = %p, want original %p", resolution.Request, request)
	}
	if got := apisixctx.GetApisixVar(request, "$consumer"); got != "" {
		t.Fatalf("consumer attachment after group failure = %#v, want none", got)
	}
	if !strings.Contains(err.Error(), "missing-group") {
		t.Fatalf("resolution error = %q, want group provenance", err)
	}
}

func TestConsumerResolutionPreservesGroupAndConsumerProvenance(t *testing.T) {
	ensureRouteStore(t)
	putHTTPAllowlistResource(t, "consumer_groups", "provenance-group", []byte(`{
		"plugins": {
			"request-id": {},
			"proxy-rewrite": {"headers": {"set": {"X-Provenance": "group"}}},
			"jwt-auth": {"key": "group-key"},
			"jwe-decrypt": {"key": "group-jwe"}
		}
	}`))

	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	resolver := builder.resolveConsumerBindings(builder.pluginRouteContext(resource.Route{ID: "provenance-route"}))
	request := consumerResolutionRequest(resource.Consumer{
		Username: "provenance-consumer",
		GroupID:  "provenance-group",
		Plugins: map[string]resource.PluginConfig{
			"proxy-rewrite": map[string]any{
				"headers": map[string]any{"set": map[string]any{"X-Provenance": "consumer"}},
			},
			"basic-auth": map[string]any{"username": "ignored", "password": "ignored"},
		},
	})

	resolution, err := resolver(request)
	if err != nil {
		t.Fatalf("resolveConsumerBindings() error = %v", err)
	}
	if !resolution.Resolved || resolution.Request == nil {
		t.Fatalf("resolution = %#v, want resolved request", resolution)
	}
	if got := apisixctx.GetApisixVar(request, "$consumer_name"); got != "provenance-consumer" {
		t.Fatalf("consumer name = %#v, want attached consumer", got)
	}
	if got := request.Header.Get("X-Consumer-Username"); got != "provenance-consumer" {
		t.Fatalf("consumer header = %q, want attached consumer", got)
	}

	provenance := make(map[string]plugin.ResourceProvenance, len(resolution.Bindings))
	for _, binding := range resolution.Bindings {
		provenance[binding.Plugin.GetName()] = binding.Provenance
		if binding.Scope != plugin.ScopeConsumer {
			t.Fatalf("binding %q scope = %d, want consumer scope", binding.Plugin.GetName(), binding.Scope)
		}
	}
	if got, want := provenance["request-id"], (plugin.ResourceProvenance{Kind: plugin.ResourceConsumerGroup, ID: "provenance-group"}); got != want {
		t.Fatalf("group provenance = %#v, want %#v", got, want)
	}
	if got, want := provenance["proxy-rewrite"], (plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "provenance-consumer"}); got != want {
		t.Fatalf("consumer provenance = %#v, want %#v", got, want)
	}
	if _, ok := provenance["jwt-auth"]; ok {
		t.Fatal("credential-only jwt-auth binding returned")
	}
	if _, ok := provenance["jwe-decrypt"]; ok {
		t.Fatal("credential-only jwe-decrypt binding returned")
	}
}

func TestAuthenticatedRouteOverwritesForgedConsumerHeader(t *testing.T) {
	tests := []struct {
		name         string
		consumerID   string
		consumerJSON string
		pluginName   string
		setAuth      func(*http.Request)
	}{
		{
			name:         "basic-auth",
			consumerID:   "route-basic-header",
			consumerJSON: `{"username":"route-basic-header","plugins":{"basic-auth":{"username":"route-basic-header","password":"route-basic-secret"}}}`,
			pluginName:   "basic-auth",
			setAuth: func(request *http.Request) {
				encoded := base64.StdEncoding.EncodeToString([]byte("route-basic-header:route-basic-secret"))
				request.Header.Set("Authorization", "Basic "+encoded)
			},
		},
		{
			name:         "key-auth",
			consumerID:   "route-key-header",
			consumerJSON: `{"username":"route-key-header","plugins":{"key-auth":{"key":"route-key-secret"}}}`,
			pluginName:   "key-auth",
			setAuth: func(request *http.Request) {
				request.Header.Set("apikey", "route-key-secret")
			},
		},
		{
			name:         "jwt-auth",
			consumerID:   "route-jwt-header",
			consumerJSON: `{"username":"route-jwt-header","plugins":{"jwt-auth":{"key":"route-jwt-key","secret":"route-jwt-secret","algorithm":"HS256"}}}`,
			pluginName:   "jwt-auth",
			setAuth: func(request *http.Request) {
				request.Header.Set("Authorization", "Bearer "+signedScopedJWT(t, "route-jwt-key", "route-jwt-secret"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			putHTTPAllowlistResource(t, "consumers", test.consumerID, []byte(test.consumerJSON))

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

			builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
			t.Cleanup(builder.Stop)
			handler, err := builder.buildHandlerStrict(resource.Route{
				ID:  "route-header-" + test.name,
				Uri: "/route-header-" + test.name,
				Plugins: map[string]resource.PluginConfig{
					test.pluginName: map[string]any{},
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

func TestConsumerResolutionCacheClonesBindingsAndSeparatesReloads(t *testing.T) {
	ensureRouteStore(t)
	putHTTPAllowlistResource(t, "consumer_groups", "cache-group", []byte(`{"plugins":{"request-id":{}}}`))

	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	resolver := builder.resolveConsumerBindings(builder.pluginRouteContext(resource.Route{ID: "cache-route"}))
	consumer := resource.Consumer{Username: "cache-consumer", GroupID: "cache-group"}
	first, err := resolver(consumerResolutionRequest(consumer))
	if err != nil {
		t.Fatalf("first resolveConsumerBindings() error = %v", err)
	}
	if len(first.Bindings) != 1 {
		t.Fatalf("first bindings = %d, want 1", len(first.Bindings))
	}
	first.Bindings[0] = plugin.Binding{}
	second, err := resolver(consumerResolutionRequest(consumer))
	if err != nil {
		t.Fatalf("second resolveConsumerBindings() error = %v", err)
	}
	if len(second.Bindings) != 1 || second.Bindings[0].Plugin == nil {
		t.Fatalf("second bindings = %#v, want independent cached clone", second.Bindings)
	}
	if first.CacheKey != second.CacheKey {
		t.Fatalf("zero-digest cache keys differ: %#v != %#v", first.CacheKey, second.CacheKey)
	}

	putHTTPAllowlistResource(t, "consumer_groups", "cache-group-reload", []byte(`{"plugins":{"request-id":{}}}`))
	reloaded := consumer
	reloaded.GroupID = "cache-group-reload"
	third, err := resolver(consumerResolutionRequest(reloaded))
	if err != nil {
		t.Fatalf("reloaded resolveConsumerBindings() error = %v", err)
	}
	if third.CacheKey == second.CacheKey {
		t.Fatal("cache key reused across group reload identity")
	}
}

func TestConsumerResolutionColdCacheInitializesStatefulBindingOnce(t *testing.T) {
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	marker := &coordinatedJSONValue{
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		release:       make(chan struct{}),
	}
	consumer := resource.Consumer{
		Username:     "cold-cache-consumer",
		ConfigDigest: [32]byte{1},
		Plugins: map[string]resource.PluginConfig{
			"limit-count": map[string]any{
				"count":         1,
				"time_window":   60,
				"key":           "remote_addr",
				"rejected_code": http.StatusTooManyRequests,
			},
			"proxy-rewrite": map[string]any{
				"headers": map[string]any{
					"set": map[string]any{"X-Cold-Cache": marker},
				},
			},
		},
	}
	resolver := builder.resolveConsumerBindings(builder.pluginRouteContext(resource.Route{ID: "cold-cache-route"}))
	type result struct {
		resolution plugin.ConsumerResolution
		err        error
	}
	firstDone := make(chan result, 1)
	go func() {
		resolution, err := resolver(consumerResolutionRequest(consumer))
		firstDone <- result{resolution: resolution, err: err}
	}()
	select {
	case <-marker.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first cold-cache initialization did not reach coordinated parse")
	}

	secondDone := make(chan result, 1)
	go func() {
		resolution, err := resolver(consumerResolutionRequest(consumer))
		secondDone <- result{resolution: resolution, err: err}
	}()
	select {
	case <-marker.secondStarted:
		close(marker.release)
		t.Fatal("same-key cold-cache initialization ran twice")
	case <-time.After(100 * time.Millisecond):
	}
	close(marker.release)

	first := <-firstDone
	second := <-secondDone
	if first.err != nil || second.err != nil {
		t.Fatalf("cold-cache resolutions = (%v, %v), want no errors", first.err, second.err)
	}
	if len(first.resolution.Bindings) != 2 || len(second.resolution.Bindings) != 2 {
		t.Fatalf(
			"cold-cache binding counts = %d/%d, want two",
			len(first.resolution.Bindings),
			len(second.resolution.Bindings),
		)
	}
	firstPlugins := make(map[string]plugin.Plugin, len(first.resolution.Bindings))
	for _, binding := range first.resolution.Bindings {
		firstPlugins[binding.Plugin.GetName()] = binding.Plugin
	}
	secondPlugins := make(map[string]plugin.Plugin, len(second.resolution.Bindings))
	for _, binding := range second.resolution.Bindings {
		secondPlugins[binding.Plugin.GetName()] = binding.Plugin
	}
	for _, name := range []string{"limit-count", "proxy-rewrite"} {
		if firstPlugins[name] == nil || secondPlugins[name] == nil {
			t.Fatalf("cold-cache bindings missing %q: %#v/%#v", name, firstPlugins, secondPlugins)
		}
		if firstPlugins[name] != secondPlugins[name] {
			t.Fatalf("cold-cache plugin %q initialized as distinct instances", name)
		}
	}
	if got := marker.callCount(); got != 1 {
		t.Fatalf("coordinated config parse calls = %d, want one", got)
	}

	limitCount := firstPlugins["limit-count"]
	for i, want := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		request := consumerResolutionRequest(consumer)
		request.RemoteAddr = "198.51.100.10:1234"
		recorder := httptest.NewRecorder()
		limitCount.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Fatalf("stateful limit-count request %d status = %d, want %d", i+1, recorder.Code, want)
		}
	}
}

func TestConsumerResolutionColdCacheSharesTransientErrorAndRetries(t *testing.T) {
	transientErr := errors.New("transient consumer plugin initialization failure")
	marker := &coordinatedFlakyJSONValue{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     transientErr,
	}
	consumer := resource.Consumer{
		Username:     "transient-cache-consumer",
		ConfigDigest: [32]byte{2},
		Plugins: map[string]resource.PluginConfig{
			"limit-count": map[string]any{
				"count":         1,
				"time_window":   60,
				"key":           "remote_addr",
				"rejected_code": http.StatusTooManyRequests,
			},
			"proxy-rewrite": map[string]any{
				"headers": map[string]any{
					"set": map[string]any{"X-Transient-Cache": marker},
				},
			},
		},
	}
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	resolver := builder.resolveConsumerBindings(builder.pluginRouteContext(resource.Route{ID: "transient-cache-route"}))
	type result struct {
		resolution plugin.ConsumerResolution
		err        error
	}
	firstDone := make(chan result, 1)
	go func() {
		resolution, err := resolver(consumerResolutionRequest(consumer))
		firstDone <- result{resolution: resolution, err: err}
	}()
	select {
	case <-marker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first transient initialization did not reach coordinated parse")
	}

	secondCalling := make(chan struct{})
	secondDone := make(chan result, 1)
	go func() {
		close(secondCalling)
		resolution, err := resolver(consumerResolutionRequest(consumer))
		secondDone <- result{resolution: resolution, err: err}
	}()
	<-secondCalling
	time.Sleep(50 * time.Millisecond)
	close(marker.release)

	first := <-firstDone
	second := <-secondDone
	if !errors.Is(first.err, transientErr) || !errors.Is(second.err, transientErr) {
		t.Fatalf("shared transient errors = (%v, %v), want %v", first.err, second.err, transientErr)
	}
	if first.err != second.err {
		t.Fatalf("concurrent transient errors are not the same result: %p != %p", first.err, second.err)
	}
	if got := marker.callCount(); got != 1 {
		t.Fatalf("concurrent transient initialization calls = %d, want one", got)
	}
	if got := len(builder.stoppers); got != 0 {
		t.Fatalf("stoppers retained after transient initialization error = %d, want zero", got)
	}

	third, err := resolver(consumerResolutionRequest(consumer))
	if err != nil {
		t.Fatalf("retry after transient initialization error = %v", err)
	}
	if len(third.Bindings) != 2 || third.Bindings[0].Plugin == nil || third.Bindings[1].Plugin == nil {
		t.Fatalf("retry bindings = %#v, want initialized limit-count and proxy-rewrite", third.Bindings)
	}
	if got := marker.callCount(); got != 2 {
		t.Fatalf("initialization calls after retry = %d, want two", got)
	}
	if got := len(builder.stoppers); got != 1 {
		t.Fatalf("stoppers after successful retry = %d, want one", got)
	}
}

func TestConsumerResolutionColdCachePublishesPanicAndRetries(t *testing.T) {
	marker := &coordinatedPanicJSONValue{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	consumer := resource.Consumer{
		Username:     "panic-cache-consumer",
		ConfigDigest: [32]byte{3},
		Plugins: map[string]resource.PluginConfig{
			"limit-count": map[string]any{
				"count":         1,
				"time_window":   60,
				"key":           "remote_addr",
				"rejected_code": http.StatusTooManyRequests,
			},
			"proxy-rewrite": map[string]any{
				"headers": map[string]any{
					"set": map[string]any{"X-Panic-Cache": marker},
				},
			},
		},
	}
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	resolver := builder.resolveConsumerBindings(builder.pluginRouteContext(resource.Route{ID: "panic-cache-route"}))
	ownerPanic := make(chan any, 1)
	go func() {
		defer func() { ownerPanic <- recover() }()
		_, _ = resolver(consumerResolutionRequest(consumer))
	}()
	select {
	case <-marker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("panic initialization did not reach coordinated parse")
	}

	type result struct {
		resolution plugin.ConsumerResolution
		err        error
	}
	waiterCalling := make(chan struct{})
	waiterDone := make(chan result, 1)
	go func() {
		close(waiterCalling)
		resolution, err := resolver(consumerResolutionRequest(consumer))
		waiterDone <- result{resolution: resolution, err: err}
	}()
	<-waiterCalling
	time.Sleep(50 * time.Millisecond)
	close(marker.release)

	select {
	case recovered := <-ownerPanic:
		if recovered != coordinatedConsumerInitializationPanic {
			t.Fatalf("owner panic = %#v, want %#v", recovered, coordinatedConsumerInitializationPanic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("panic owner did not terminate")
	}
	select {
	case waiter := <-waiterDone:
		if waiter.err == nil || !strings.Contains(waiter.err.Error(), "consumer plugin initialization panicked") {
			t.Fatalf("panic waiter error = %v, want bounded initialization panic", waiter.err)
		}
		if waiter.resolution.Resolved {
			t.Fatal("panic waiter returned a resolved consumer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("panic waiter remained blocked")
	}
	if got := len(builder.stoppers); got != 0 {
		t.Fatalf("stoppers retained after initialization panic = %d, want zero", got)
	}

	retried, err := resolver(consumerResolutionRequest(consumer))
	if err != nil {
		t.Fatalf("retry after initialization panic = %v", err)
	}
	if len(retried.Bindings) != 2 {
		t.Fatalf("retry bindings after initialization panic = %d, want two", len(retried.Bindings))
	}
	if got := marker.callCount(); got != 2 {
		t.Fatalf("panic initialization calls after retry = %d, want two", got)
	}
	if got := len(builder.stoppers); got != 1 {
		t.Fatalf("stoppers after panic retry success = %d, want one", got)
	}
}

func TestPlan14V2DeferredLegacyEffectiveWinnerRunsOnce(t *testing.T) {
	order := []string{}
	route := consumerAccessLegacy("route", 100, &order)
	consumer := consumerAccessLegacy("consumer", 50, &order)
	pipeline := plugin.NewRequestPipeline(
		[]plugin.Binding{
			plugin.BindPlugin(
				"same-name",
				route,
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route"},
			),
		},
		func(r *http.Request) (plugin.ConsumerResolution, error) {
			return plugin.ConsumerResolution{
				Request:  r,
				Resolved: true,
				Bindings: []plugin.Binding{
					plugin.BindPlugin(
						"same-name",
						consumer,
						plugin.ScopeConsumer,
						plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "consumer"},
					),
				},
			}, nil
		},
	)
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { order = append(order, "terminal") })).
		ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	if got, want := order, []string{"consumer", "terminal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("effective deferred legacy order = %#v, want %#v", got, want)
	}
}

func TestPlan14V2DeferredLegacyDoesNotObserveAuthFailure(t *testing.T) {
	order := []string{}
	auth := consumerAccessPhase("jwt-auth", func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
		order = append(order, "auth")
		return base.StopRequest(r)
	})
	legacy := consumerAccessLegacy("legacy", 10, &order)
	pipeline := plugin.NewRequestPipeline(
		[]plugin.Binding{
			plugin.BindPlugin(
				"jwt-auth",
				auth,
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "auth"},
			),
			plugin.BindPlugin(
				"legacy",
				legacy,
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "legacy"},
			),
		},
		func(*http.Request) (plugin.ConsumerResolution, error) {
			t.Fatal("resolver called after auth failure")
			return plugin.ConsumerResolution{}, nil
		},
	)
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { order = append(order, "terminal") })).
		ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	if got, want := order, []string{"auth"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("auth failure order = %#v, want %#v", got, want)
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

func consumerResolutionRequest(consumer resource.Consumer) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/consumer", nil)
	request = apisixctx.WithApisixVars(request, nil)
	return apisixctx.WithAuthenticationState(
		request,
		apisixctx.NewAuthenticationState("jwt-auth", consumer),
	)
}

type coordinatedJSONValue struct {
	firstStarted  chan struct{}
	secondStarted chan struct{}
	release       chan struct{}
	mu            sync.Mutex
	calls         int
}

type coordinatedFlakyJSONValue struct {
	started chan struct{}
	release chan struct{}
	err     error
	mu      sync.Mutex
	calls   int
}

const coordinatedConsumerInitializationPanic = "coordinated consumer initialization panic"

type coordinatedPanicJSONValue struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (v *coordinatedPanicJSONValue) MarshalJSON() ([]byte, error) {
	v.mu.Lock()
	v.calls++
	call := v.calls
	v.mu.Unlock()
	if call == 1 {
		close(v.started)
		<-v.release
		panic(coordinatedConsumerInitializationPanic)
	}
	return []byte(`"value"`), nil
}

func (v *coordinatedPanicJSONValue) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func (v *coordinatedFlakyJSONValue) MarshalJSON() ([]byte, error) {
	v.mu.Lock()
	v.calls++
	call := v.calls
	v.mu.Unlock()
	if call == 1 {
		close(v.started)
		<-v.release
		return nil, v.err
	}
	return []byte(`"value"`), nil
}

func (v *coordinatedFlakyJSONValue) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func (v *coordinatedJSONValue) MarshalJSON() ([]byte, error) {
	v.mu.Lock()
	v.calls++
	call := v.calls
	v.mu.Unlock()
	switch call {
	case 1:
		close(v.firstStarted)
		<-v.release
	case 2:
		close(v.secondStarted)
		<-v.release
	}
	return []byte(`"value"`), nil
}

func (v *coordinatedJSONValue) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

type consumerAccessLegacyPlugin struct {
	name     string
	priority int
	order    *[]string
}

func (p *consumerAccessLegacyPlugin) Init() error               { return nil }
func (p *consumerAccessLegacyPlugin) PostInit() error           { return nil }
func (p *consumerAccessLegacyPlugin) Config() any               { return nil }
func (p *consumerAccessLegacyPlugin) GetSchema() string         { return "" }
func (p *consumerAccessLegacyPlugin) GetMetadataSchema() string { return "" }
func (p *consumerAccessLegacyPlugin) GetPriority() int          { return p.priority }
func (p *consumerAccessLegacyPlugin) GetName() string           { return p.name }
func (p *consumerAccessLegacyPlugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*p.order = append(*p.order, p.name)
		next.ServeHTTP(w, r)
	})
}

func consumerAccessLegacy(name string, priority int, order *[]string) plugin.Plugin {
	return &consumerAccessLegacyPlugin{name: name, priority: priority, order: order}
}

type consumerAccessPhasePlugin struct {
	consumerAccessLegacyPlugin
	phase func(http.ResponseWriter, *http.Request) base.RequestPhaseResult
}

func (p *consumerAccessPhasePlugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	return p.phase(w, r)
}

func consumerAccessPhase(
	name string,
	phase func(http.ResponseWriter, *http.Request) base.RequestPhaseResult,
) plugin.Plugin {
	return &consumerAccessPhasePlugin{
		consumerAccessLegacyPlugin: consumerAccessLegacyPlugin{name: name, priority: 100},
		phase:                      phase,
	}
}

var (
	_ plugin.Plugin           = (*consumerAccessLegacyPlugin)(nil)
	_ base.RequestPhasePlugin = (*consumerAccessPhasePlugin)(nil)
)
