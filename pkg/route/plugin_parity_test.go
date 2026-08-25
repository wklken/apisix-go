package route

import (
	stdjson "encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	pluginpkg "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestWorkflowRouteChainAllowsNonMatchingRequest(t *testing.T) {
	handler := buildWorkflowRouteChain(t)

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/allowed", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
	if res.Header().Get("X-Route-Fallback") != "reached" {
		t.Fatal("fallback handler was not reached for a non-matching workflow rule")
	}
}

func TestBuildHandlerStrictRunsConsumerRestrictionFromAuthenticatedConsumer(t *testing.T) {
	if err := metrics.Init(nil); err != nil {
		t.Fatalf("metrics.Init() error = %v", err)
	}
	consumer := resource.Consumer{
		Username: "restricted-basic-user",
		Plugins: map[string]resource.PluginConfig{
			"basic-auth": map[string]any{
				"username": "restricted-basic-user",
				"password": "secret",
			},
			"consumer-restriction": map[string]any{
				"type":          "route_id",
				"blacklist":     []any{"consumer-plugin-route"},
				"rejected_code": http.StatusUnauthorized,
			},
		},
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
		ID:  "consumer-plugin-route",
		Uri: "/restricted",
		Plugins: map[string]resource.PluginConfig{
			"basic-auth": map[string]any{},
			"consumer-restriction": map[string]any{
				"type":          "route_id",
				"whitelist":     []any{"consumer-plugin-route"},
				"rejected_code": http.StatusForbidden,
			},
		},
		Upstream: resource.Upstream{
			Type:   "roundrobin",
			Scheme: upstreamURL.Scheme,
			Nodes:  []resource.Node{{Host: upstreamURL.Hostname(), Port: port, Weight: 1}},
		},
	}
	lookup := &testConsumerLookup{byKey: map[string]resource.Consumer{
		"restricted-basic-user": consumer,
	}}
	authBinding := testPluginBindingWithDependencies(
		t,
		"basic-auth",
		map[string]any{},
		route,
		base.Dependencies{
			Consumers: lookup,
		},
	)
	routeRestriction := testPluginBinding(
		t,
		"consumer-restriction",
		route.Plugins["consumer-restriction"],
		route,
	)
	consumerRestriction := testPluginBindingWithDependenciesScope(
		t,
		"consumer-restriction",
		consumer.Plugins["consumer-restriction"],
		route,
		base.Dependencies{Consumers: lookup},
		pluginpkg.ScopeConsumer,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceConsumer, ID: consumer.Username},
	)
	handler := testPreparedConsumerHandler(t, route, map[string]PreparedConsumerRecord{
		consumer.Username: {Consumer: consumer, Bindings: []pluginpkg.Binding{consumerRestriction}},
	}, []pluginpkg.Binding{authBinding, routeRestriction}, upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/restricted", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{"$route_id": "consumer-plugin-route"})
	req.Header.Set("Authorization", "Basic cmVzdHJpY3RlZC1iYXNpYy11c2VyOnNlY3JldA==")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want consumer restriction rejection", response.Code)
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"message":"The route_id is forbidden."}` {
		t.Fatalf("body = %q", got)
	}
}

func TestNormalizePluginResourceContextUsesInitializedServiceConfig(t *testing.T) {
	context := pluginRouteContext{
		route: resource.Route{Plugins: map[string]resource.PluginConfig{
			"opa": map[string]any{"host": "http://opa.test", "policy": "echo"},
		}},
		service: resource.Service{Plugins: map[string]resource.PluginConfig{
			"key-auth": map[string]any{},
		}},
	}
	normalized := map[string]any{
		"header":           "apikey",
		"query":            "apikey",
		"hide_credentials": false,
	}

	context = normalizePluginResourceContext(context, "key-auth", normalized)
	data, err := apisixjson.Marshal(context.service.Plugins["key-auth"])
	if err != nil {
		t.Fatalf("marshal normalized service plugin: %v", err)
	}
	if got := string(data); got != `{"header":"apikey","hide_credentials":false,"query":"apikey"}` {
		t.Fatalf("normalized service key-auth = %s", got)
	}
	if _, ok := context.route.Plugins["key-auth"]; ok {
		t.Fatal("normalized service config was incorrectly added to the route")
	}
}

func TestWorkflowRouteChainRejectsMatchingRequest(t *testing.T) {
	handler := buildWorkflowRouteChain(t)

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/blocked", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusForbidden)
	}
	if res.Header().Get("X-Route-Fallback") != "" {
		t.Fatal("fallback handler was reached after workflow rejection")
	}
}

func buildWorkflowRouteChain(t *testing.T) http.Handler {
	t.Helper()

	return buildRoutePluginChain(t, "workflow", map[string]any{
		"rules": []any{
			map[string]any{
				"case": []any{[]any{"uri", "==", "/blocked"}},
				"actions": []any{
					[]any{"return", map[string]any{"code": http.StatusForbidden}},
				},
			},
		},
	})
}

func TestOASValidatorRouteChainAcceptsValidRequest(t *testing.T) {
	handler := buildRoutePluginChain(t, "oas-validator", map[string]any{
		"spec": routeOASSpec,
	})

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets?id=7", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
	if res.Header().Get("X-Route-Fallback") != "reached" {
		t.Fatal("fallback handler was not reached for a valid OAS request")
	}
}

func TestOASValidatorRouteChainRejectsInvalidRequest(t *testing.T) {
	handler := buildRoutePluginChain(t, "oas-validator", map[string]any{
		"spec": routeOASSpec,
	})

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if res.Header().Get("X-Route-Fallback") != "" {
		t.Fatal("fallback handler was reached after OAS validation rejection")
	}
}

func TestBodyTransformerRouteChainTransformsValidRequest(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "body-transformer", map[string]any{
		"request": map[string]any{
			"input_format": "json",
			"template":     `{"name":"{{name}}"}`,
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"name":"dog"}` {
			t.Fatalf("transformed body = %q, want %q", body, `{"name":"dog"}`)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(
		http.MethodPost,
		"http://route.example.com/pets",
		strings.NewReader(`{"name":"dog"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestBodyTransformerRouteChainRejectsMalformedRequest(t *testing.T) {
	handler := buildRoutePluginChain(t, "body-transformer", map[string]any{
		"request": map[string]any{
			"input_format": "json",
			"template":     `{"name":"{{name}}"}`,
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://route.example.com/pets",
		strings.NewReader(`{"name":`),
	)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if res.Header().Get("X-Route-Fallback") != "" {
		t.Fatal("fallback handler was reached after body-transformer rejection")
	}
}

func TestDataMaskRouteChainPreservesQuery(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "data-mask", map[string]any{
		"request": []any{
			map[string]any{
				"type":   "query",
				"name":   "token",
				"action": "replace",
				"value":  "***",
			},
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("token"); got != "secret" {
			t.Fatalf("upstream query token = %q, want original secret", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets?token=secret", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestDataMaskRouteChainPreservesMalformedJSON(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "data-mask", map[string]any{
		"request": []any{
			map[string]any{
				"type":        "body",
				"body_format": "json",
				"name":        "$.token",
				"action":      "replace",
				"value":       "***",
			},
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if string(body) != `{"token":` {
			t.Fatalf("upstream body = %q, want original malformed JSON", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(
		http.MethodPost,
		"http://route.example.com/pets",
		strings.NewReader(`{"token":`),
	)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestProxyBufferingRouteChainPropagatesFlushControl(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "proxy-buffering", map[string]any{
		"disable_proxy_buffering": true,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !proxy_buffering.GetDisableProxyBuffering(r) {
			t.Fatal("proxy-buffering disable flag was not propagated")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestProxyControlRouteChainPropagatesRequestBuffering(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "proxy-control", map[string]any{
		"request_buffering": false,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if proxy_control.GetRequestBuffering(r) {
			t.Fatal("proxy-control request buffering flag was not disabled")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestRequestValidationRouteChainAcceptsRequiredHeader(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "request-validation", map[string]any{
		"header_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"X-Token": map[string]any{"type": "string"},
			},
			"required": []any{"X-Token"},
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "secret" {
			t.Fatalf("X-Token = %q, want secret", r.Header.Get("X-Token"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	req.Header.Set("X-Token", "secret")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestRequestValidationRouteChainRejectsMissingHeader(t *testing.T) {
	handler := buildRoutePluginChain(t, "request-validation", map[string]any{
		"header_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"X-Token": map[string]any{"type": "string"},
			},
			"required": []any{"X-Token"},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if res.Header().Get("X-Route-Fallback") != "" {
		t.Fatal("fallback handler was reached after request-validation rejection")
	}
}

func TestDegraphqlRouteChainRewritesGETRequest(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "degraphql", map[string]any{
		"query": "{ pets }",
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != "{ pets }" {
			t.Fatalf("query = %q, want { pets }", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/graphql?ignored=true", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestDegraphqlRouteChainRejectsUnsupportedMethod(t *testing.T) {
	handler := buildRoutePluginChain(t, "degraphql", map[string]any{
		"query": "{ pets }",
	})

	req := httptest.NewRequest(http.MethodDelete, "http://route.example.com/graphql", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusMethodNotAllowed)
	}
	if res.Header().Get("X-Route-Fallback") != "" {
		t.Fatal("fallback handler was reached after degraphql method rejection")
	}
}

func TestTrafficSplitRouteChainSetsSelectedUpstream(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "traffic-split", map[string]any{
		"rules": []any{
			map[string]any{
				"weighted_upstreams": []any{
					map[string]any{
						"weight": 1,
						"upstream": map[string]any{
							"scheme": "http",
							"nodes": []any{
								map[string]any{"host": "127.0.0.1", "port": 18080, "weight": 1},
							},
						},
					},
				},
			},
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		override := traffic_split.GetOverride(r)
		if override == nil || override.Host != "127.0.0.1:18080" {
			t.Fatalf("traffic-split override = %#v, want 127.0.0.1:18080", override)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestTrafficSplitRouteChainRejectsMissingUpstream(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "traffic-split", map[string]any{
		"rules": []any{
			map[string]any{
				"weighted_upstreams": []any{
					map[string]any{"upstream_id": "missing-route-upstream", "weight": 1},
				},
			},
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("fallback handler was reached with a missing traffic-split upstream")
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusInternalServerError)
	}
}

func TestGRPCWebRouteChainTransformsValidRequest(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(
		t,
		"grpc-web",
		nil,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Content-Type") != "application/grpc" {
				t.Fatalf(
					"upstream content type = %q, want application/grpc",
					r.Header.Get("Content-Type"),
				)
			}
			w.Header().Set("Content-Type", "application/grpc")
			_, _ = w.Write([]byte{0, 0, 0, 0, 0})
		}),
	)

	req := httptest.NewRequest(
		http.MethodPost,
		"http://route.example.com/service/Method",
		strings.NewReader("\x00\x00\x00\x00\x00"),
	)
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if res.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS origin = %q, want *", res.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestGRPCWebRouteChainRejectsUnsupportedMethod(t *testing.T) {
	handler := buildRoutePluginChain(t, "grpc-web", nil)

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/service/Method", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusMethodNotAllowed)
	}
	if res.Header().Get("X-Route-Fallback") != "" {
		t.Fatal("fallback handler was reached after grpc-web method rejection")
	}
}

func TestLimitReqRouteChainAllowsThenRejectsSameKey(t *testing.T) {
	handler := buildRoutePluginChain(t, "limit-req", map[string]any{
		"rate":          1,
		"burst":         0,
		"key":           "remote_addr",
		"nodelay":       true,
		"rejected_code": http.StatusTooManyRequests,
	})

	first := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	first.RemoteAddr = "192.0.2.10:1234"
	firstRes := httptest.NewRecorder()
	handler.ServeHTTP(firstRes, first)
	if firstRes.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRes.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	second.RemoteAddr = "192.0.2.10:5678"
	secondRes := httptest.NewRecorder()
	handler.ServeHTTP(secondRes, second)
	if secondRes.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRes.Code, http.StatusTooManyRequests)
	}
}

func TestLimitConnRouteChainRejectsConcurrentSameKey(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	handler := buildRoutePluginChainWithFallback(t, "limit-conn", map[string]any{
		"conn":               1,
		"burst":              0,
		"default_conn_delay": 0.001,
		"key":                "remote_addr",
		"rejected_code":      http.StatusTooManyRequests,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	first := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	first.RemoteAddr = "192.0.2.20:1234"
	go func() {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, first)
		firstDone <- res
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first concurrent request")
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	second := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	second.RemoteAddr = "192.0.2.20:5678"
	go func() {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, second)
		secondDone <- res
	}()

	var secondRes *httptest.ResponseRecorder
	select {
	case secondRes = <-secondDone:
	case <-time.After(time.Second):
		close(release)
		<-firstDone
		<-secondDone
		t.Fatal("timed out waiting for concurrent limit rejection")
	}
	if secondRes.Code != http.StatusTooManyRequests {
		close(release)
		<-firstDone
		t.Fatalf("second status = %d, want %d", secondRes.Code, http.StatusTooManyRequests)
	}

	close(release)
	firstRes := <-firstDone
	if firstRes.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRes.Code, http.StatusNoContent)
	}
}

func TestResponseRewriteRouteChainRewritesUpstreamResponse(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "response-rewrite", map[string]any{
		"status_code": http.StatusCreated,
		"headers": map[string]any{
			"set": map[string]any{"X-Rewritten": "yes"},
		},
		"body": "rewritten",
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream"))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
	}
	if res.Header().Get("X-Rewritten") != "yes" {
		t.Fatalf("X-Rewritten = %q, want yes", res.Header().Get("X-Rewritten"))
	}
	if res.Body.String() != "rewritten" {
		t.Fatalf("body = %q, want rewritten", res.Body.String())
	}
}

func TestForwardAuthRouteChainAllowsAndCopiesHeaders(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization = %q, want Bearer token", r.Header.Get("Authorization"))
		}
		w.Header().Set("X-User-ID", "alice")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(auth.Close)

	handler := buildRoutePluginChainWithFallback(t, "forward-auth", map[string]any{
		"uri":              auth.URL,
		"request_headers":  []any{"Authorization"},
		"upstream_headers": []any{"X-User-ID"},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-User-ID") != "alice" {
			t.Fatalf("X-User-ID = %q, want alice", r.Header.Get("X-User-ID"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	req.Header.Set("Authorization", "Bearer token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestForwardAuthRouteChainRejectsDeniedRequest(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Auth-Reason", "denied")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("nope"))
	}))
	t.Cleanup(auth.Close)

	handler := buildRoutePluginChainWithFallback(t, "forward-auth", map[string]any{
		"uri":            auth.URL,
		"client_headers": []any{"X-Auth-Reason"},
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("fallback handler was reached after forward-auth rejection")
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if res.Header().Get("X-Auth-Reason") != "denied" {
		t.Fatalf("X-Auth-Reason = %q, want denied", res.Header().Get("X-Auth-Reason"))
	}
}

func TestJWTAuthRouteChainUsesAnonymousConsumer(t *testing.T) {
	consumer := resource.Consumer{Username: "route-jwt-anonymous"}
	routeResource := resource.Route{
		ID: "jwt-auth-anonymous-route", Uri: "/",
		Plugins: map[string]resource.PluginConfig{"jwt-auth": map[string]any{
			"anonymous_consumer": consumer.Username,
		}},
	}
	binding := testPluginBindingWithDependencies(t, "jwt-auth", map[string]any{
		"anonymous_consumer": "route-jwt-anonymous",
	}, routeResource, base.Dependencies{Consumers: &testConsumerLookup{
		byKey: map[string]resource.Consumer{consumer.Username: consumer},
	}})
	handler := withRequestPipeline(
		pluginpkg.BuildPluginChain(binding.Plugin),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := apisixctx.GetApisixVar(r, "$consumer_name"); got != "route-jwt-anonymous" {
				t.Fatalf("consumer name = %v, want route-jwt-anonymous", got)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestJWTAuthRouteChainRejectsMissingToken(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(
		t,
		"jwt-auth",
		nil,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("fallback handler was reached without a JWT token")
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
	}
}

func TestChaitinWAFRouteChainAllowsAndBlocks(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decision := map[string]any{"status": http.StatusOK}
		if r.Header.Get("X-WAF-Decision") == "deny" {
			decision = map[string]any{"status": http.StatusForbidden, "event_id": "route-event"}
		}
		if err := stdjson.NewEncoder(w).Encode(decision); err != nil {
			t.Errorf("encode WAF decision: %v", err)
		}
	}))
	t.Cleanup(waf.Close)

	wafURL, err := url.Parse(waf.URL)
	if err != nil {
		t.Fatalf("parse WAF URL: %v", err)
	}
	port, err := strconv.Atoi(wafURL.Port())
	if err != nil {
		t.Fatalf("parse WAF port: %v", err)
	}
	config := map[string]any{
		"mode": "block",
		"nodes": []any{
			map[string]any{"host": wafURL.Hostname(), "port": port},
		},
	}
	handler := buildRoutePluginChainWithFallback(
		t,
		"chaitin-waf",
		config,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	)

	allowed := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	allowed.Header.Set("X-WAF-Decision", "allow")
	allowedRes := httptest.NewRecorder()
	handler.ServeHTTP(allowedRes, allowed)
	if allowedRes.Code != http.StatusNoContent {
		t.Fatalf("allowed status = %d, want %d", allowedRes.Code, http.StatusNoContent)
	}

	blocked := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	blocked.Header.Set("X-WAF-Decision", "deny")
	blockedRes := httptest.NewRecorder()
	handler.ServeHTTP(blockedRes, blocked)
	if blockedRes.Code != http.StatusForbidden {
		t.Fatalf("blocked status = %d, want %d", blockedRes.Code, http.StatusForbidden)
	}
}

func TestErrorPageRouteChainRewritesConfiguredMetadata(t *testing.T) {
	metadata, err := runtime.NewMetadataView(map[string][]byte{
		"error-page": []byte(
			`{"enable":true,"error_404":{"body":"route 404","content_type":"text/plain"}}`,
		),
	})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	routeResource := resource.Route{ID: "error-page-route-test", Uri: "/"}
	binding := testPluginBindingForSourceWithDependencies(
		t,
		"error-page",
		nil,
		pluginpkg.ScopeRoute,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: routeResource.ID},
		routeResource,
		resource.Service{},
		"127.0.0.1:9080",
		base.Dependencies{Config: testEffectiveConfig(), Metadata: metadata},
	)
	handler := withRequestPipeline(
		pluginpkg.BuildPluginChain(binding.Plugin),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("original"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNotFound)
	}
	if res.Body.String() != "route 404" {
		t.Fatalf("body = %q, want route 404", res.Body.String())
	}
}

func TestExitTransformerRouteChainRemapsLocalError(t *testing.T) {
	handler := buildRoutePluginChainWithFallback(t, "exit-transformer", map[string]any{
		"functions": []any{
			`return (function(code, body, header) if code == 500 then return 503, body, header end return code, body, header end)(...)`,
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("failure"))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/pets", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestProxyMirrorRouteChainPreservesMainRequestAndMirrors(t *testing.T) {
	seen := make(chan struct{}, 1)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read mirrored body: %v", err)
		}
		if string(body) != "payload" {
			t.Errorf("mirrored body = %q, want payload", body)
		}
		if r.URL.Path != "/shadow" || r.URL.RawQuery != "x=1" {
			t.Errorf("mirrored target = %s?%s, want /shadow?x=1", r.URL.Path, r.URL.RawQuery)
		}
		seen <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(mirror.Close)

	handler := buildRoutePluginChainWithFallback(t, "proxy-mirror", map[string]any{
		"host": mirror.URL,
		"path": "/shadow",
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read main body: %v", err)
		}
		if string(body) != "payload" {
			t.Fatalf("main body = %q, want payload", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(
		http.MethodPost,
		"http://route.example.com/original?x=1",
		strings.NewReader("payload"),
	)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxy-mirror request")
	}
}

func buildRoutePluginChain(t *testing.T, name string, config map[string]any) http.Handler {
	t.Helper()
	return buildRoutePluginChainWithFallback(
		t,
		name,
		config,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Route-Fallback", "reached")
			w.WriteHeader(http.StatusNoContent)
		}),
	)
}

func buildRoutePluginChainWithFallback(
	t *testing.T,
	name string,
	config map[string]any,
	fallback http.Handler,
) http.Handler {
	t.Helper()
	routeResource := resource.Route{
		ID: name + "-route-test", Uri: "/",
		Plugins: map[string]resource.PluginConfig{name: config},
	}
	binding := testScopedSecretPluginBinding(t, name, config, routeResource)
	return withRequestPipeline(pluginpkg.BuildPluginChain(binding.Plugin), fallback)
}

const routeOASSpec = `{"openapi":"3.0.0","info":{"title":"route-test","version":"1.0.0"},"paths":{"/pets":{"get":{"parameters":[{"name":"id","in":"query","required":true,"schema":{"type":"integer"}}],"responses":{"204":{"description":"ok"}}}}}}`
