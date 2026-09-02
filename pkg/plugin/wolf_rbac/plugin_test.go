package wolf_rbac

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
)

type wolfConsumerLookup struct {
	mu     sync.RWMutex
	byKey  map[string]resource.Consumer
	calls  []string
	closed bool
}

type cancelAwareWolfConsumerLookup struct {
	entered chan context.Context
	release chan struct{}
}

func (*cancelAwareWolfConsumerLookup) ConsumerByPluginKey(string, string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (*cancelAwareWolfConsumerLookup) ConsumerByID(string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (*cancelAwareWolfConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func (lookup *cancelAwareWolfConsumerLookup) UseConsumerCredential(
	requestContext context.Context,
	_ string,
	_ string,
	_ base.ConsumerCredentialUse,
) (bool, error) {
	lookup.entered <- requestContext
	select {
	case <-requestContext.Done():
		return false, requestContext.Err()
	case <-lookup.release:
		return false, nil
	}
}

func (lookup *wolfConsumerLookup) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.calls = append(lookup.calls, plugin+"\x00"+key)
	if lookup.closed {
		return resource.Consumer{}, false
	}
	consumer, ok := lookup.byKey[key]
	return consumer, ok
}

func (*wolfConsumerLookup) ConsumerByID(string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (*wolfConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func (lookup *wolfConsumerLookup) close() {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.closed = true
	lookup.byKey = nil
}

func wolfBoundConsumer(username, appID, server string, verify bool) resource.Consumer {
	return resource.Consumer{
		Username: username,
		Plugins: map[string]resource.PluginConfig{name: map[string]any{
			"appid": appID, "server": server, "header_prefix": "X-Lookup-", "ssl_verify": verify,
		}},
	}
}

func newLookupTestPlugin(t *testing.T, cfg Config, lookup base.ConsumerLookup) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	p.SetPublicAPIRegistry(public_api.NewRegistry())
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p
}

type wolfTestFixture struct {
	sync.Mutex
	byKey map[string]resource.Consumer
}

var wolfTestFixtures sync.Map

func wolfFixtureFor(t *testing.T) *wolfTestFixture {
	t.Helper()
	fixture := &wolfTestFixture{byKey: map[string]resource.Consumer{}}
	actual, loaded := wolfTestFixtures.LoadOrStore(t, fixture)
	if !loaded {
		t.Cleanup(func() { wolfTestFixtures.Delete(t) })
	}
	return actual.(*wolfTestFixture)
}

func addWolfConsumer(t *testing.T, username, appid, server string) {
	t.Helper()
	fixture := wolfFixtureFor(t)
	fixture.Lock()
	defer fixture.Unlock()
	fixture.byKey[appid] = resource.Consumer{
		Username: username,
		Plugins: map[string]resource.PluginConfig{name: map[string]any{
			"appid": appid, "server": server, "header_prefix": "X-", "ssl_verify": false,
		}},
	}
}

func newTestPlugin(t *testing.T, cfg Config, registries ...*public_api.Registry) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	fixture := wolfFixtureFor(t)
	fixture.Lock()
	byKey := make(map[string]resource.Consumer, len(fixture.byKey))
	maps.Copy(byKey, fixture.byKey)
	fixture.Unlock()
	p.SetDependencies(base.Dependencies{Consumers: &wolfConsumerLookup{byKey: byKey}})
	registry := public_api.NewRegistry()
	if len(registries) > 0 && registries[0] != nil {
		registry = registries[0]
	}
	p.SetPublicAPIRegistry(registry)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func newTestPublicAPIRegistry(t *testing.T) *public_api.Registry {
	t.Helper()

	registry := public_api.NewRegistry()
	newTestPlugin(t, Config{}, registry)
	return registry
}

func TestPostInitAppliesOfficialDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.AppID != "unset" {
		t.Fatalf("appid = %q, want unset", p.config.AppID)
	}
	if p.config.Server != "http://127.0.0.1:12180" {
		t.Fatalf("server = %q, want official default", p.config.Server)
	}
	if p.config.HeaderPrefix != "X-" {
		t.Fatalf("header_prefix = %q, want X-", p.config.HeaderPrefix)
	}
	if p.config.SSLVerify == nil || !*p.config.SSLVerify {
		t.Fatalf("ssl_verify = %v, want true", p.config.SSLVerify)
	}
}

func TestClientForConfigTLSSecurityMatrix(t *testing.T) {
	wolf := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"userInfo": map[string]any{"id": "tls-user", "username": "tls-user"},
			},
		})
	}))
	t.Cleanup(wolf.Close)

	cases := []struct {
		name               string
		pluginConfig       Config
		injectedClient     bool
		consumerConfig     consumerConfig
		applyPluginDefault bool
		wantErr            bool
	}{
		{
			name:           "default nil rejects untrusted server",
			consumerConfig: consumerConfig{Server: wolf.URL},
			wantErr:        true,
		},
		{
			name:           "explicit false succeeds",
			consumerConfig: consumerConfig{Server: wolf.URL, SSLVerify: new(false)},
		},
		{
			name:           "trusted injected transport succeeds with verification enabled",
			injectedClient: true,
			consumerConfig: consumerConfig{Server: wolf.URL, SSLVerify: new(true)},
		},
		{
			name:               "consumer nil inherits route true",
			injectedClient:     true,
			consumerConfig:     consumerConfig{Server: wolf.URL},
			applyPluginDefault: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{config: tc.pluginConfig}
			if tc.injectedClient {
				p.client = wolf.Client()
			}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err != nil {
				t.Fatalf("PostInit() error = %v", err)
			}

			cfg := tc.consumerConfig
			if tc.applyPluginDefault {
				cfg.applyDefaults(p.config)
				if cfg.SSLVerify == nil || !*cfg.SSLVerify {
					t.Fatalf("consumer ssl_verify = %v, want inherited true", cfg.SSLVerify)
				}
			}

			request := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
			status, _, _, err := p.checkPermission(request, cfg, rbacToken{
				AppID:     "app-a",
				WolfToken: "token-a",
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("checkPermission() error = nil, want untrusted TLS rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("checkPermission() error = %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
		})
	}
}

func TestHandlerChecksWolfPermissionAndAttachesConsumer(t *testing.T) {
	requests := make(chan *http.Request, 1)
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		if r.URL.Path != "/wolf/rbac/access_check" {
			t.Fatalf("path = %q, want access_check", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"userInfo": map[string]any{
					"id":       "u-1",
					"username": "alice",
					"nickname": "Alice Zhang",
				},
			},
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-user", "app-a", wolf.URL)
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders/1?debug=true", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("Authorization", "V1#app-a#wolf-token")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "wolf-user" {
			t.Fatalf("consumer_name = %v, want wolf-user", got)
		}
		if got := r.Header.Get("X-UserId"); got != "u-1" {
			t.Fatalf("upstream X-UserId = %q, want u-1", got)
		}
		if got := r.Header.Get("X-Username"); got != "alice" {
			t.Fatalf("upstream X-Username = %q, want alice", got)
		}
		if got := r.Header.Get("X-Nickname"); got != url.QueryEscape("Alice Zhang") {
			t.Fatalf("upstream X-Nickname = %q, want escaped nickname", got)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-UserId") != "u-1" {
		t.Fatalf("response X-UserId = %q, want u-1", rr.Header().Get("X-UserId"))
	}

	select {
	case got := <-requests:
		query := got.URL.Query()
		if query.Get("appID") != "app-a" {
			t.Fatalf("appID = %q, want app-a", query.Get("appID"))
		}
		if query.Get("resName") != "/orders/1" {
			t.Fatalf("resName = %q, want path", query.Get("resName"))
		}
		if query.Get("action") != http.MethodPost {
			t.Fatalf("action = %q, want POST", query.Get("action"))
		}
		if query.Get("clientIP") != "203.0.113.10" {
			t.Fatalf("clientIP = %q, want remote IP", query.Get("clientIP"))
		}
		if got.Header.Get("X-Rbac-Token") != "wolf-token" {
			t.Fatalf("x-rbac-token = %q, want wolf-token", got.Header.Get("X-Rbac-Token"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Wolf access_check request")
	}
}

func TestHandlerUsesInjectedWolfConsumerLookupAuthoritatively(t *testing.T) {
	competingWolf := newWolfLookupServer(t, "competing-route-wolf")
	lookupWolf := newWolfLookupServer(t, "lookup-wolf")
	lookup := &wolfConsumerLookup{byKey: map[string]resource.Consumer{
		"lookup-app": wolfBoundConsumer("lookup-wolf-consumer", "lookup-app", lookupWolf.URL, true),
	}}
	p := newLookupTestPlugin(t, Config{Server: competingWolf.URL}, lookup)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Set("Authorization", "V1#lookup-app#wolf-token")
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "lookup-wolf-consumer" {
			t.Fatalf("consumer_name = %v, want lookup-wolf-consumer", got)
		}
		if got := r.Header.Get("X-Lookup-Username"); got != "lookup-wolf" {
			t.Fatalf("consumer server/header override = %q, want lookup-wolf", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", response.Code, response.Body.String())
	}
	lookup.mu.RLock()
	if len(lookup.calls) != 1 || lookup.calls[0] != name+"\x00lookup-app" {
		t.Fatalf("lookup calls = %#v, want exact factory/appid", lookup.calls)
	}
	lookup.mu.RUnlock()

	miss := newLookupTestPlugin(t, Config{}, &wolfConsumerLookup{})
	missResponse := performRequest(t, miss, "V1#lookup-app#wolf-token")
	if missResponse.Code != http.StatusUnauthorized ||
		!strings.Contains(missResponse.Body.String(), "Invalid appid in rbac token") {
		t.Fatalf("lookup miss response = %d/%q, want invalid-appid 401", missResponse.Code, missResponse.Body.String())
	}
}

func TestWolfConsumerLookupStopsWhenRequestIsCanceled(t *testing.T) {
	tests := []struct {
		name    string
		request func(context.Context) *http.Request
		serve   func(*Plugin, http.ResponseWriter, *http.Request)
	}{
		{
			name: "protected request",
			request: func(requestContext context.Context) *http.Request {
				request := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil).
					WithContext(requestContext)
				request.Header.Set("Authorization", "V1#cancel-app#wolf-token")
				return request
			},
			serve: func(plugin *Plugin, response http.ResponseWriter, request *http.Request) {
				plugin.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
					ServeHTTP(response, request)
			},
		},
		{
			name: "login public API",
			request: func(requestContext context.Context) *http.Request {
				request := httptest.NewRequest(
					http.MethodPost,
					WolfLoginURI,
					strings.NewReader(`{"appid":"cancel-app","username":"admin","password":"secret"}`),
				).WithContext(requestContext)
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			serve: func(plugin *Plugin, response http.ResponseWriter, request *http.Request) {
				plugin.handleLogin(response, request)
			},
		},
		{
			name: "token public API",
			request: func(requestContext context.Context) *http.Request {
				request := httptest.NewRequest(http.MethodGet, WolfUserInfoURI, nil).WithContext(requestContext)
				request.Header.Set("Authorization", "V1#cancel-app#wolf-token")
				return request
			},
			serve: func(plugin *Plugin, response http.ResponseWriter, request *http.Request) {
				plugin.handleUserInfo(response, request)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := &cancelAwareWolfConsumerLookup{
				entered: make(chan context.Context, 1),
				release: make(chan struct{}),
			}
			t.Cleanup(func() {
				select {
				case <-lookup.release:
				default:
					close(lookup.release)
				}
			})
			plugin := newLookupTestPlugin(t, Config{}, lookup)
			requestContext, cancel := context.WithCancel(context.Background())
			request := test.request(requestContext)
			done := make(chan struct{})
			go func() {
				defer close(done)
				test.serve(plugin, httptest.NewRecorder(), request)
			}()

			select {
			case <-lookup.entered:
			case <-time.After(time.Second):
				t.Fatal("consumer lookup did not start")
			}
			cancel()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("consumer lookup did not stop after request cancellation")
			}
		})
	}
}

func TestWolfConsumerLookupsAreGenerationIsolated(t *testing.T) {
	firstWolf := newWolfLookupServer(t, "wolf-generation-n")
	secondWolf := newWolfLookupServer(t, "wolf-generation-n-plus-one")
	firstLookup := &wolfConsumerLookup{byKey: map[string]resource.Consumer{
		"overlap-app": wolfBoundConsumer("wolf-consumer-n", "overlap-app", firstWolf.URL, true),
	}}
	secondLookup := &wolfConsumerLookup{byKey: map[string]resource.Consumer{
		"overlap-app": wolfBoundConsumer("wolf-consumer-n-plus-one", "overlap-app", secondWolf.URL, true),
	}}
	first := newLookupTestPlugin(t, Config{}, firstLookup)
	second := newLookupTestPlugin(t, Config{}, secondLookup)
	assertConsumer := func(p *Plugin, wantConsumer, wantUser string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
		request = ctx.WithApisixVars(request, map[string]string{})
		request.Header.Set("Authorization", "V1#overlap-app#wolf-token")
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != wantConsumer {
				t.Errorf("consumer_name = %v, want %s", got, wantConsumer)
			}
			if got := r.Header.Get("X-Lookup-Username"); got != wantUser {
				t.Errorf("wolf username = %q, want %s", got, wantUser)
			}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204; body=%s", response.Code, response.Body.String())
		}
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() { assertConsumer(first, "wolf-consumer-n", "wolf-generation-n") })
		group.Go(func() {
			assertConsumer(second, "wolf-consumer-n-plus-one", "wolf-generation-n-plus-one")
		})
	}
	group.Wait()
	firstLookup.close()
	assertConsumer(second, "wolf-consumer-n-plus-one", "wolf-generation-n-plus-one")
}

func newWolfLookupServer(t *testing.T, username string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{"userInfo": map[string]any{
				"id": username + "-id", "username": username,
			}},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestHandlerRejectsMissingAndInvalidToken(t *testing.T) {
	p := newTestPlugin(t, Config{})

	missing := performRequest(t, p, "")
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", missing.Code)
	}
	if got := missing.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("missing token Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := missing.Header().Get("X-Content-Type-Options"); got != "" {
		t.Fatalf("missing token X-Content-Type-Options = %q, want absent", got)
	}
	if got := missing.Body.String(); got != "{\"message\":\"Missing rbac token in request\"}\n" {
		t.Fatalf("missing token body = %q", missing.Body.String())
	}

	invalid := performRequest(t, p, "invalid-token")
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", invalid.Code)
	}
	if got := invalid.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("invalid token Content-Type = %q, want application/json; charset=UTF-8", got)
	}
	if got := invalid.Body.String(); got != `{"message":"invalid rbac token: parse failed"}` {
		t.Fatalf("invalid token body = %q", invalid.Body.String())
	}
}

func TestWolfRBACUserInfoPublicAPIMissingTokenMatchesAPISIX317(t *testing.T) {
	registry := public_api.NewRegistry()
	newTestPlugin(t, Config{}, registry)
	handler := registry.Lookup(http.MethodGet, WolfUserInfoURI)
	if handler == nil {
		t.Fatal("wolf user-info public API is not registered")
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, WolfUserInfoURI, nil))

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "" {
		t.Fatalf("X-Content-Type-Options = %q, want absent", got)
	}
	if got := response.Body.String(); got != "{\"message\":\"Missing rbac token in request\"}\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestHandlerPropagatesWolfDenial(t *testing.T) {
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"reason": "permission denied",
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-denied-user", "app-denied", wolf.URL)
	p := newTestPlugin(t, Config{})

	res := performRequest(t, p, "V1#app-denied#wolf-token")
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if !strings.Contains(res.Body.String(), "permission denied") {
		t.Fatalf("body = %q, want denial reason", res.Body.String())
	}
}

func TestHandlerRejectsSuccessfulHTTPResponsesWithoutWolfPermission(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "business denial",
			body: map[string]any{
				"ok":     false,
				"reason": "permission denied",
				"data": map[string]any{
					"userInfo": map[string]any{"id": "u-denied", "username": "denied"},
				},
			},
		},
		{
			name: "missing ok",
			body: map[string]any{
				"reason": "permission response is incomplete",
				"data": map[string]any{
					"userInfo": map[string]any{"id": "u-missing-ok", "username": "missing-ok"},
				},
			},
		},
		{
			name: "empty user info",
			body: map[string]any{
				"ok":     true,
				"reason": "permission response has no identity",
				"data":   map[string]any{"userInfo": map[string]any{}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			t.Cleanup(wolf.Close)
			appid := "app-invalid-permission-" + strings.ReplaceAll(tc.name, " ", "-")
			addWolfConsumer(t, "wolf-invalid-permission-"+appid, appid, wolf.URL)
			p := newTestPlugin(t, Config{})

			req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
			req = ctx.WithApisixVars(req, map[string]string{})
			req.Header.Set("Authorization", "V1#"+appid+"#wolf-token")
			rr := httptest.NewRecorder()
			nextCalls := 0
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				nextCalls++
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
			}
			if nextCalls != 0 {
				t.Fatalf("next calls = %d, want 0", nextCalls)
			}
			for _, header := range []string{"X-UserId", "X-Username", "X-Nickname"} {
				if got := rr.Header().Get(header); got != "" {
					t.Fatalf("response %s = %q, want empty", header, got)
				}
				if got := req.Header.Get(header); got != "" {
					t.Fatalf("request %s = %q, want empty", header, got)
				}
			}
		})
	}
}

func TestHandlerClearsForgedIdentityHeaders(t *testing.T) {
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"reason": "permission denied",
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-forged-header-user", "app-forged-header", wolf.URL)
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "V1#app-forged-header#wolf-token")
	for _, name := range []string{"X-UserId", "X-Username", "X-Nickname"} {
		req.Header.Set(name, "attacker")
	}
	rr := httptest.NewRecorder()
	for _, name := range []string{"X-UserId", "X-Username", "X-Nickname"} {
		rr.Header().Set(name, "attacker")
	}
	nextCalled := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if nextCalled {
		t.Fatal("next handler was called after Wolf denied the request")
	}
	for _, name := range []string{"X-UserId", "X-Username", "X-Nickname"} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("request %s = %q, want forged header removed", name, got)
		}
		if got := rr.Header().Get(name); got != "" {
			t.Fatalf("response %s = %q, want forged header removed", name, got)
		}
	}
}

func TestHandlerRejectsEmptyUserInfo(t *testing.T) {
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"userInfo": map[string]any{},
			},
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-empty-user-info", "app-empty-user-info", wolf.URL)
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "V1#app-empty-user-info#wolf-token")
	for _, name := range []string{"X-UserId", "X-Username", "X-Nickname"} {
		req.Header.Set(name, "attacker")
	}
	rr := httptest.NewRecorder()
	for _, name := range []string{"X-UserId", "X-Username", "X-Nickname"} {
		rr.Header().Set(name, "attacker")
	}
	nextCalled := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if nextCalled {
		t.Fatal("next handler was called for an empty Wolf userInfo response")
	}
	for _, name := range []string{"X-UserId", "X-Username", "X-Nickname"} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("request %s = %q, want forged header removed", name, got)
		}
		if got := rr.Header().Get(name); got != "" {
			t.Fatalf("response %s = %q, want forged header removed", name, got)
		}
	}
}

func TestFetchTokenFromQueryAndCookie(t *testing.T) {
	queryRequest := httptest.NewRequest(http.MethodGet, "/?rbac_token=V1%23app%23query", nil)
	if got := fetchRBACToken(queryRequest); got != "V1#app#query" {
		t.Fatalf("query token = %q", got)
	}
	if !ctx.IsSensitiveQueryName(queryRequest, "rbac_token") {
		t.Fatal("wolf-rbac did not register rbac_token query key")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "x-rbac-token", Value: "V1#app#cookie"})
	if got := fetchRBACToken(req); got != "V1#app#cookie" {
		t.Fatalf("cookie token = %q", got)
	}
}

func TestCheckPermissionHonorsSSLVerifyFalse(t *testing.T) {
	wolf := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"userInfo": map[string]any{"id": "tls-user", "username": "tls-user"},
			},
		})
	}))
	t.Cleanup(wolf.Close)

	p := newTestPlugin(t, Config{})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	status, _, _, err := p.checkPermission(request, consumerConfig{
		Server:    wolf.URL,
		SSLVerify: new(false),
	}, rbacToken{AppID: "app-a", WolfToken: "token-a"})
	if err != nil {
		t.Fatalf("checkPermission() error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

func TestHandlerRetriesTransientWolfServerFailure(t *testing.T) {
	requests := 0
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"userInfo": map[string]any{"id": "u-retry", "username": "retry-user"},
			},
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-retry-user", "app-retry", wolf.URL)
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "V1#app-retry#wolf-token")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if requests != 2 {
		t.Fatalf("wolf requests = %d, want 2", requests)
	}
}

func TestHandlerUsesRealIPFromRequestContext(t *testing.T) {
	requests := make(chan *http.Request, 1)
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"userInfo": map[string]any{"id": "u-real-ip", "username": "real-ip-user"},
			},
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-real-ip-user", "app-real-ip", wolf.URL)
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/hello", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctx.RemoteAddrKey, "192.0.2.10"))
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "V1#app-real-ip#wolf-token")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	select {
	case got := <-requests:
		if clientIP := got.URL.Query().Get("clientIP"); clientIP != "192.0.2.10" {
			t.Fatalf("clientIP = %q, want request lifecycle real IP", clientIP)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Wolf access_check request")
	}
}

func TestHandlerStopsAfterThreeWolfServerFailures(t *testing.T) {
	requests := 0
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-retry-exhausted-user", "app-retry-exhausted", wolf.URL)
	p := newTestPlugin(t, Config{})

	res := performRequest(t, p, "V1#app-retry-exhausted#wolf-token")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", res.Code, res.Body.String())
	}
	if got := strings.TrimSpace(res.Body.String()); got != `{"message":"request to wolf-server failed, status:500"}` {
		t.Fatalf("body = %q, want exhausted retry diagnostic", got)
	}
	if requests != wolfRetryMax {
		t.Fatalf("wolf requests = %d, want %d", requests, wolfRetryMax)
	}
}

func TestPostInitRegistersWolfRBACPublicAPIs(t *testing.T) {
	registry := newTestPublicAPIRegistry(t)
	for _, endpoint := range []struct {
		method string
		uri    string
	}{
		{http.MethodPost, WolfLoginURI},
		{http.MethodPut, WolfChangePasswordURI},
		{http.MethodGet, WolfUserInfoURI},
	} {
		if handler := registry.Lookup(endpoint.method, endpoint.uri); handler == nil {
			t.Fatalf("public API %s %s is not registered", endpoint.method, endpoint.uri)
		}
	}
}

func TestWolfRBACPublicAPIsUseTheirOwnGenerationRegistry(t *testing.T) {
	wolfA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"token":    "token-a",
				"userInfo": map[string]any{"username": "alice"},
			},
		})
	}))
	t.Cleanup(wolfA.Close)
	wolfB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"token":    "token-b",
				"userInfo": map[string]any{"username": "bob"},
			},
		})
	}))
	t.Cleanup(wolfB.Close)
	addWolfConsumer(t, "wolf-isolation-a", "app-isolation-a", wolfA.URL)
	addWolfConsumer(t, "wolf-isolation-b", "app-isolation-b", wolfB.URL)

	registryA := newTestPublicAPIRegistry(t)
	registryB := newTestPublicAPIRegistry(t)

	for _, test := range []struct {
		name     string
		registry *public_api.Registry
		appid    string
		want     string
	}{
		{name: "first", registry: registryA, appid: "app-isolation-a", want: "token-a"},
		{name: "second", registry: registryB, appid: "app-isolation-b", want: "token-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := test.registry.Lookup(http.MethodPost, WolfLoginURI)
			if handler == nil {
				t.Fatal("wolf login public API is not registered")
			}
			request := httptest.NewRequest(
				http.MethodPost,
				WolfLoginURI,
				strings.NewReader(`{"appid":"`+test.appid+`","username":"admin","password":"secret"}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got := body["rbac_token"]; got != "V1#"+test.appid+"#"+test.want {
				t.Fatalf("rbac_token = %v, want backend %s", got, test.want)
			}
		})
	}
}

func TestRouteInstancesDoNotOverwriteGenerationWolfPublicAPIs(t *testing.T) {
	var backendAHits int
	wolfA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendAHits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"token":    "token-a",
				"userInfo": map[string]any{"username": "alice"},
			},
		})
	}))
	t.Cleanup(wolfA.Close)
	var backendBHits int
	wolfB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendBHits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"token":    "token-b",
				"userInfo": map[string]any{"username": "bob"},
			},
		})
	}))
	t.Cleanup(wolfB.Close)
	addWolfConsumer(t, "wolf-generation-owner", "app-generation-owner", "")

	registry := public_api.NewRegistry()
	newTestPlugin(t, Config{Server: wolfA.URL}, registry)
	conflicting := &Plugin{config: Config{Server: wolfB.URL}}
	conflicting.SetPublicAPIRegistry(registry)
	if err := conflicting.Init(); err != nil {
		t.Fatalf("conflicting Init() error = %v", err)
	}
	if err := conflicting.PostInit(); err == nil {
		t.Fatal("conflicting PostInit() error = nil, want conflicting public API configuration error")
	}

	handler := registry.Lookup(http.MethodPost, WolfLoginURI)
	request := httptest.NewRequest(
		http.MethodPost,
		WolfLoginURI,
		strings.NewReader(`{"appid":"app-generation-owner","username":"admin","password":"secret"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if backendAHits != 1 || backendBHits != 0 {
		t.Fatalf("route backend hits = (%d, %d), want (1, 0)", backendAHits, backendBHits)
	}
}

func TestWolfRBACLoginPublicAPIForwardsCredentialsAndWrapsToken(t *testing.T) {
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wolf/rbac/login.rest" {
			t.Fatalf("request = %s %s, want POST login.rest", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["username"] != "alice" || body["password"] != "secret" {
			t.Fatalf("body = %#v, want login credentials", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"token":    "wolf-token",
				"userInfo": map[string]any{"username": "alice"},
			},
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-login-user", "app-login", wolf.URL)

	registry := newTestPublicAPIRegistry(t)
	handler := registry.Lookup(http.MethodPost, WolfLoginURI)
	if handler == nil {
		t.Fatal("wolf login public API is not registered")
	}
	req := httptest.NewRequest(
		http.MethodPost,
		WolfLoginURI,
		strings.NewReader(`{"appid":"app-login","username":"alice","password":"secret"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["rbac_token"] != "V1#app-login#wolf-token" {
		t.Fatalf("rbac_token = %v, want wrapped Wolf token", response["rbac_token"])
	}
}

func TestWolfRBACLoginBusinessDenialReturnsGenericFailureWithStatusOK(t *testing.T) {
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wolf/rbac/login.rest" {
			t.Fatalf("request = %s %s, want POST login.rest", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"reason": "ERR_PASSWORD_ERROR",
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-login-denied-user", "app-login-denied", wolf.URL)

	registry := newTestPublicAPIRegistry(t)
	handler := registry.Lookup(http.MethodPost, WolfLoginURI)
	req := httptest.NewRequest(
		http.MethodPost,
		WolfLoginURI,
		strings.NewReader("appid=app-login-denied&username=admin&password=wrong-password"),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"request to wolf-server failed!"}` {
		t.Fatalf("body = %q, want generic Wolf failure", got)
	}
}

func TestRequestArgumentsPreservesMoreThanOneHundredFormFields(t *testing.T) {
	values := url.Values{
		"oldPassword": {"123456"},
		"newPassword": {"abcdef"},
	}
	for index := 1; index <= 100; index++ {
		values.Set(fmt.Sprintf("test%d", index), "test")
	}
	req := httptest.NewRequest(http.MethodPut, WolfChangePasswordURI, strings.NewReader(values.Encode()))

	args, err := requestArguments(req)
	if err != nil {
		t.Fatalf("requestArguments() error = %v", err)
	}
	if len(args) != 102 {
		t.Fatalf("argument count = %d, want 102", len(args))
	}
	if args["test100"] != "test" || args["oldPassword"] != "123456" || args["newPassword"] != "abcdef" {
		t.Fatalf("arguments = %#v, want all password and overflow fields", args)
	}
}

func performRequest(t *testing.T, p *Plugin, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)
	return rr
}

func TestSetUserHeadersRejectsUnsupportedIdentityFieldTypes(t *testing.T) {
	plugin := &Plugin{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	if err := plugin.setUserHeaders(w, r, "X-", map[string]any{
		"id": map[string]any{"nested": true}, "username": "alice",
	}); err == nil {
		t.Fatal("setUserHeaders() with map id = nil error, want unsupported-type error")
	}

	if err := plugin.setUserHeaders(w, r, "X-", map[string]any{
		"id": []any{"1"}, "username": "alice",
	}); err == nil {
		t.Fatal("setUserHeaders() with slice id = nil error, want unsupported-type error")
	}
}

func TestSetUserHeadersRejectsIncompleteIdentity(t *testing.T) {
	plugin := &Plugin{}
	cases := []struct {
		name     string
		userInfo map[string]any
	}{
		{name: "empty user info", userInfo: map[string]any{}},
		{name: "missing id", userInfo: map[string]any{"username": "alice"}},
		{name: "blank id", userInfo: map[string]any{"id": "  ", "username": "alice"}},
		{name: "missing username", userInfo: map[string]any{"id": "u-1"}},
		{name: "blank username", userInfo: map[string]any{"id": "u-1", "username": "  "}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if err := plugin.setUserHeaders(w, r, "X-", tc.userInfo); err == nil {
				t.Fatalf("setUserHeaders() error = nil, want incomplete identity error")
			}
		})
	}
}

func TestSetUserHeadersAcceptsScalarIdentityFields(t *testing.T) {
	plugin := &Plugin{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	if err := plugin.setUserHeaders(w, r, "X-", map[string]any{
		"id": int64(7), "username": "alice", "nickname": "ali",
	}); err != nil {
		t.Fatalf("setUserHeaders() error = %v", err)
	}
	if got := w.Header().Get("X-UserId"); got != "7" {
		t.Fatalf("X-UserId = %q, want 7", got)
	}
	if got := w.Header().Get("X-Username"); got != "alice" {
		t.Fatalf("X-Username = %q", got)
	}
	if got := w.Header().Get("X-Nickname"); got != "ali" {
		t.Fatalf("X-Nickname = %q", got)
	}
}
