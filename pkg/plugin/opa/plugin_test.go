package opa

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitWarnsOnlyForInsecureHost(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		wantWarn bool
	}{
		{name: "http", host: "http://127.0.0.1:8181", wantWarn: true},
		{name: "https", host: "https://127.0.0.1:8181"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var warnings []logger.Entry
			stop := logger.ReplaceObserver("opa-security-warning-"+test.name, func(entry logger.Entry) {
				if entry.Level == "WARN" && strings.Contains(entry.Message, "opa host") {
					warnings = append(warnings, entry)
				}
			})
			defer stop()

			_ = newTestPlugin(t, Config{Host: test.host, Policy: "example/allow"})
			if got := len(warnings); got != 0 && !test.wantWarn {
				t.Fatalf("warnings = %#v, want none for TLS host", warnings)
			}
			if got := len(warnings); got != 1 && test.wantWarn {
				t.Fatalf("warnings = %#v, want one insecure host warning", warnings)
			}
		})
	}
}

func TestSchemaMatchesAPISIX317SanityMatrix(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{name: "valid", config: map[string]any{"host": "http://127.0.0.1:8181", "policy": "example/allow"}},
		{name: "missing policy", config: map[string]any{"host": "http://127.0.0.1:8181"}, wantErr: true},
		{name: "numeric host", config: map[string]any{"host": 3233, "policy": "example/allow"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := util.Validate(test.config, p.GetSchema())
			if test.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want APISIX 3.17 schema rejection")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want APISIX 3.17 schema acceptance", err)
			}
		})
	}
}

func TestHandlerAllowsRequestAndSendsOPAInput(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodPost {
			t.Fatalf("method = %q, want POST", got)
		}
		if got := r.URL.Path; got != "/v1/data/http/authz" {
			t.Fatalf("path = %q, want /v1/data/http/authz", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode OPA body: %v", err)
		}
		input := body["input"].(map[string]any)
		if got := input["type"]; got != "http" {
			t.Fatalf("input.type = %v, want http", got)
		}
		request := input["request"].(map[string]any)
		if got := request["method"]; got != http.MethodGet {
			t.Fatalf("request.method = %v, want GET", got)
		}
		if got := request["path"]; got != "/get" {
			t.Fatalf("request.path = %v, want /get", got)
		}
		if got := request["host"]; got != "example.com" {
			t.Fatalf("request.host = %v, want example.com", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:   opa.URL,
		Policy: "http/authz",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsNon2xxOPAResponseBeforeAllowBody(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:   opa.URL,
		Policy: "authz",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for a non-2xx OPA response")
	})

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestHandlerDoesNotFollowOPAResponseRedirect(t *testing.T) {
	var redirectedHits atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedHits.Add(1)
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	defer redirected.Close()
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL, http.StatusFound)
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{Host: opa.URL, Policy: "authz"})
	res := performRequest(p, func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called for a redirected OPA response")
	})

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if got := redirectedHits.Load(); got != 0 {
		t.Fatalf("redirected OPA backend hits = %d, want 0", got)
	}
}

func TestBuildOPARequestUsesAPISIXHTTPShape(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://opa.test", Policy: "authz"})
	req := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.test:9080/get?one=1&many=b&many=a",
		nil,
	)
	req.Header.Set("X-Test", "yes")

	body, err := json.Marshal(p.buildOPARequest(req))
	if err != nil {
		t.Fatalf("marshal OPA request: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal OPA request: %v", err)
	}
	request := decoded["input"].(map[string]any)["request"].(map[string]any)
	headers := request["headers"].(map[string]any)
	if got := headers["host"]; got != "gateway.test:9080" {
		t.Fatalf("headers.host = %#v, want gateway.test:9080", got)
	}
	if got := headers["x-test"]; got != "yes" {
		t.Fatalf("headers.x-test = %#v, want yes", got)
	}
	if _, ok := headers["X-Test"]; ok {
		t.Fatalf("headers contains canonicalized X-Test key: %#v", headers)
	}
	query := request["query"].(map[string]any)
	if got := query["one"]; got != "1" {
		t.Fatalf("query.one = %#v, want scalar 1", got)
	}
	many, ok := query["many"].([]any)
	if !ok || len(many) != 2 || many[0] != "b" || many[1] != "a" {
		t.Fatalf("query.many = %#v, want [b a]", query["many"])
	}
}

func TestBuildOPARequestIncludesCompleteConsumer(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://opa.test", Policy: "authz", WithConsumer: true})
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/get", nil)
	req = apisixctx.WithApisixVars(req, nil)
	attachedConsumer := resource.Consumer{
		Username: "test-user",
		GroupID:  "group-1",
		Plugins: map[string]resource.PluginConfig{
			"key-auth": map[string]any{"key": "consumer-plugin-secret"},
		},
		Labels: map[string]any{"team": "edge"},
	}
	apisixctx.AttachConsumer(req, attachedConsumer)

	body, err := json.Marshal(p.buildOPARequest(req))
	if err != nil {
		t.Fatalf("marshal OPA request: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal OPA request: %v", err)
	}
	consumer := decoded["input"].(map[string]any)["consumer"].(map[string]any)
	labels, ok := consumer["labels"].(map[string]any)
	if consumer["username"] != "test-user" || consumer["group_id"] != "group-1" || !ok || labels["team"] != "edge" {
		t.Fatalf("consumer = %#v, want complete consumer", consumer)
	}
	plugins, ok := consumer["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("consumer = %#v, want plugin configuration", consumer)
	}
	keyAuth, ok := plugins["key-auth"].(map[string]any)
	if !ok || keyAuth["key"] != "consumer-plugin-secret" {
		t.Fatalf("consumer plugins = %#v, want complete configuration", plugins)
	}
}

func TestOPAResourceContextIncludesCompleteResourcesWithoutUpstream(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host:         "http://opa.test",
		Policy:       "authz",
		WithRoute:    true,
		WithService:  true,
		WithConsumer: true,
	})
	p.SetResourceContext(
		resource.Route{
			ID:   "route-1",
			Name: "orders-route",
			Uri:  "/orders/*",
			Plugins: map[string]resource.PluginConfig{
				"key-auth": map[string]any{"key": "route-plugin-secret"},
			},
			Upstream: resource.Upstream{
				Name: "route-upstream-secret",
				TLS:  &resource.UpstreamTLS{ClientKey: "route-client-key-secret"},
			},
		},
		resource.Service{
			ID:   "service-1",
			Name: "orders-service",
			Plugins: map[string]resource.PluginConfig{
				"basic-auth": map[string]any{"password": "service-plugin-secret"},
			},
			Upstream: resource.Upstream{
				Name: "service-upstream-secret",
				TLS:  &resource.UpstreamTLS{ClientKey: "service-client-key-secret"},
			},
		},
	)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders/1", nil)
	req = apisixctx.WithApisixVars(req, nil)
	attachedConsumer := resource.Consumer{
		Username: "test-user",
		GroupID:  "group-1",
		Plugins: map[string]resource.PluginConfig{
			"key-auth": map[string]any{"key": "consumer-plugin-secret"},
		},
		Labels: map[string]any{"team": "edge"},
	}
	apisixctx.AttachConsumer(req, attachedConsumer)

	body, err := json.Marshal(p.buildOPARequest(req))
	if err != nil {
		t.Fatalf("marshal OPA request: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal OPA request: %v", err)
	}
	input := decoded["input"].(map[string]any)
	route := input["route"].(map[string]any)
	if route["id"] != "route-1" || route["name"] != "orders-route" || route["uri"] != "/orders/*" {
		t.Fatalf("route = %#v, want complete route context", route)
	}
	if _, ok := route["upstream"]; ok {
		t.Fatalf("route = %#v, want upstream omitted", route)
	}
	routePlugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("route = %#v, want plugin configuration", route)
	}
	keyAuth, ok := routePlugins["key-auth"].(map[string]any)
	if !ok || keyAuth["key"] != "route-plugin-secret" {
		t.Fatalf("route plugins = %#v, want complete configuration", routePlugins)
	}
	service := input["service"].(map[string]any)
	if service["id"] != "service-1" || service["name"] != "orders-service" {
		t.Fatalf("service = %#v, want complete service context", service)
	}
	if _, ok := service["upstream"]; ok {
		t.Fatalf("service = %#v, want upstream omitted", service)
	}
	servicePlugins, ok := service["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("service = %#v, want plugin configuration", service)
	}
	basicAuth, ok := servicePlugins["basic-auth"].(map[string]any)
	if !ok || basicAuth["password"] != "service-plugin-secret" {
		t.Fatalf("service plugins = %#v, want complete configuration", servicePlugins)
	}
	consumer := input["consumer"].(map[string]any)
	labels, ok := consumer["labels"].(map[string]any)
	if consumer["username"] != "test-user" || consumer["group_id"] != "group-1" || !ok || labels["team"] != "edge" {
		t.Fatalf("consumer = %#v, want complete consumer", consumer)
	}
	consumerPlugins, ok := consumer["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("consumer = %#v, want plugin configuration", consumer)
	}
	keyAuth, ok = consumerPlugins["key-auth"].(map[string]any)
	if !ok || keyAuth["key"] != "consumer-plugin-secret" {
		t.Fatalf("consumer plugins = %#v, want complete configuration", consumerPlugins)
	}
	if p.route.Upstream.Name != "route-upstream-secret" || p.service.Upstream.Name != "service-upstream-secret" {
		t.Fatalf(
			"resource context mutated: route upstream=%q, service upstream=%q",
			p.route.Upstream.Name,
			p.service.Upstream.Name,
		)
	}
	if attachedConsumer.Plugins["key-auth"].(map[string]any)["key"] != "consumer-plugin-secret" {
		t.Fatalf("attached consumer mutated: %#v", attachedConsumer)
	}
}

func TestBuildOPARequestMatchesAPISIX317FactsRepeatedHeadersAndAddresses(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://opa.test", Policy: "authz"})
	req := httptest.NewRequest(http.MethodGet, "https://gateway.test:9443/get?x=1&x=2", nil)
	req.Host = "gateway.test:9443"
	req.RemoteAddr = "192.0.2.10:41000"
	req.Header.Add("X-Role", "admin")
	req.Header.Add("X-Role", "reader")
	req = req.WithContext(context.WithValue(req.Context(), apisixctx.RemoteAddrKey, "198.51.100.20"))
	req = req.WithContext(context.WithValue(req.Context(), apisixctx.RemotePortKey, "8443"))
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, testAddr("127.0.0.1:9080")))

	body, err := json.Marshal(p.buildOPARequest(req))
	if err != nil {
		t.Fatalf("marshal OPA request: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal OPA request: %v", err)
	}
	input := decoded["input"].(map[string]any)
	if _, exists := input["version"]; exists {
		t.Fatalf("input.version = %#v, want absent for APISIX 3.17 parity", input["version"])
	}
	request := input["request"].(map[string]any)
	if request["port"] != float64(9080) || request["query"].(map[string]any)["x"].([]any)[1] != "2" {
		t.Fatalf("request = %#v, want listener port and query from facts", request)
	}
	requestHeaders := request["headers"].(map[string]any)
	if !sameJSONValues(requestHeaders["x-role"], []any{"admin", "reader"}) {
		t.Fatalf("request.headers[x-role] = %#v, want repeated values", requestHeaders["x-role"])
	}
	vars := input["var"].(map[string]any)
	if vars["remote_addr"] != "198.51.100.20" || vars["remote_port"] != "8443" {
		t.Fatalf("client vars = %#v", vars)
	}
	if vars["server_addr"] != "127.0.0.1" || vars["server_port"] != "9080" {
		t.Fatalf("server vars = %#v", vars)
	}
	if _, ok := vars["timestamp"].(float64); !ok {
		t.Fatalf("timestamp = %#v, want numeric", vars["timestamp"])
	}
}

func TestHandlerRejectsWithOPAStatusReasonAndHeaders(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(
			[]byte(
				`{"result":{"allow":false,"status_code":401,"reason":"no token","headers":{"WWW-Authenticate":"Bearer"}}}`,
			),
		)
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:   opa.URL,
		Policy: "authz",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if got := res.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "no token" {
		t.Fatalf("body = %q, want no token", got)
	}
}

func TestHandlerRejectsNonTerminalOPAStatus(t *testing.T) {
	for _, code := range []int{99, 100, 600} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"result":{"allow":false,"status_code":%d,"reason":"bad status"}}`, code)
			}))
			t.Cleanup(opa.Close)

			p := newTestPlugin(t, Config{
				Host:   opa.URL,
				Policy: "authz",
			})

			res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler should not be called")
			})

			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

func TestHandlerCopiesAllowedHeadersToUpstreamAndClearsAbsentOnes(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"allow":true,"headers":{"X-User":"alice"}}}`))
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:                opa.URL,
		Policy:              "authz",
		SendHeadersUpstream: []string{"X-User", "X-Role"},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("X-Role", "client-role")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-User"); got != "alice" {
			t.Fatalf("X-User = %q, want alice", got)
		}
		if got := r.Header.Get("X-Role"); got != "" {
			t.Fatalf("X-Role = %q, want cleared", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerReturns503ForInvalidDecision(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer opa.Close()

	p := newTestPlugin(t, Config{
		Host:   opa.URL,
		Policy: "authz",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestHandlerIncludesRouteAndServiceContextWhenConfigured(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode OPA body: %v", err)
		}
		input := body["input"].(map[string]any)
		route, ok := input["route"].(map[string]any)
		if !ok || route["id"] != "route-1" || route["name"] != "orders" || route["uri"] != "/orders/*" {
			t.Fatalf("input.route = %#v, want local route context", input["route"])
		}
		service, ok := input["service"].(map[string]any)
		if !ok || service["id"] != "service-1" || service["name"] != "orders-service" {
			t.Fatalf("input.service = %#v, want local service context", input["service"])
		}
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	t.Cleanup(opa.Close)

	p := newTestPlugin(t, Config{
		Host:        opa.URL,
		Policy:      "authz",
		WithRoute:   true,
		WithService: true,
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":     "route-1",
		"$route_name":   "orders",
		"$matched_uri":  "/orders/*",
		"$service_id":   "service-1",
		"$service_name": "orders-service",
	})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerIncludesCompleteRouteAndServiceResourcesWithoutUpstream(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read OPA body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(rawBody, &body); err != nil {
			t.Fatalf("decode OPA body: %v", err)
		}
		input := body["input"].(map[string]any)
		route := input["route"].(map[string]any)
		if route["id"] != "route-1" || route["uri"] != "/orders/*" || route["name"] != "orders" {
			t.Fatalf("input.route = %#v, want complete route resource", route)
		}
		if _, ok := route["upstream"]; ok {
			t.Fatalf("input.route = %#v, want upstream omitted", route)
		}
		service := input["service"].(map[string]any)
		if service["id"] != "service-1" || service["name"] != "orders" {
			t.Fatalf("input.service = %#v, want complete service resource", service)
		}
		if _, ok := service["upstream"]; ok {
			t.Fatalf("input.service = %#v, want upstream omitted", service)
		}
		routePlugins, ok := route["plugins"].(map[string]any)
		if !ok {
			t.Fatalf("input.route = %#v, want plugin configuration", route)
		}
		keyAuth, ok := routePlugins["key-auth"].(map[string]any)
		if !ok || keyAuth["key"] != "route-plugin-secret" {
			t.Fatalf("input.route plugins = %#v, want complete configuration", routePlugins)
		}
		servicePlugins, ok := service["plugins"].(map[string]any)
		if !ok {
			t.Fatalf("input.service = %#v, want plugin configuration", service)
		}
		basicAuth, ok := servicePlugins["basic-auth"].(map[string]any)
		if !ok || basicAuth["password"] != "service-plugin-secret" {
			t.Fatalf("input.service plugins = %#v, want complete configuration", servicePlugins)
		}
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	t.Cleanup(opa.Close)

	p := newTestPlugin(t, Config{Host: opa.URL, Policy: "authz", WithRoute: true, WithService: true})
	p.SetResourceContext(
		resource.Route{
			ID:       "route-1",
			Name:     "orders",
			Uri:      "/orders/*",
			Priority: 10,
			Plugins: map[string]resource.PluginConfig{
				"key-auth": map[string]any{"key": "route-plugin-secret"},
			},
			Upstream: resource.Upstream{
				Name: "upstream-secret",
				TLS:  &resource.UpstreamTLS{ClientKey: "route-upstream-secret"},
			},
		},
		resource.Service{
			ID:              "service-1",
			Name:            "orders",
			EnableWebsocket: true,
			Plugins: map[string]resource.PluginConfig{
				"basic-auth": map[string]any{"password": "service-plugin-secret"},
			},
			Upstream: resource.Upstream{
				Name: "service-upstream-secret",
				TLS:  &resource.UpstreamTLS{ClientKey: "service-upstream-secret"},
			},
		},
	)
	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func sameJSONValues(got any, want []any) bool {
	values, ok := got.([]any)
	if !ok || len(values) != len(want) {
		return false
	}
	for i := range values {
		if values[i] != want[i] {
			return false
		}
	}
	return true
}

type testAddr string

func (a testAddr) Network() string { return "tcp" }

func (a testAddr) String() string { return string(a) }

func performRequest(p *Plugin, upstream func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?x=1", nil)
	req.Header.Set("X-Test", "yes")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(upstream)).ServeHTTP(rr, req)
	return rr
}
