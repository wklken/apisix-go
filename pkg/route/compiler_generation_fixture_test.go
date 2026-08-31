package route_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/secret"
)

func TestPreparedGenerationCompilesRouteWithoutBuilder(t *testing.T) {
	prepared := compileRouteGeneration(t, []generation.Resource{{
		Key: generation.ResourceKey{Kind: "routes", ID: "compiled"},
		Value: []byte(`{
			"id":"compiled",
			"uri":"/compiled",
			"upstream":{"scheme":"http","nodes":{"127.0.0.1:1":1}}
		}`),
	}}, nil)
	response := httptest.NewRecorder()
	prepared.HTTP().Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/compiled", nil),
	)
	if response.Code == http.StatusNotFound {
		t.Fatalf("compiled route status = %d, want registered handler", response.Code)
	}
}

func TestPreparedGenerationPublicAPIExposesConfiguredPrometheusEndpoint(t *testing.T) {
	for _, test := range []struct {
		name           string
		enableExporter bool
		wantStatus     int
	}{
		{name: "dedicated exporter disabled", wantStatus: http.StatusOK},
		{name: "dedicated exporter enabled", enableExporter: true, wantStatus: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := compileRouteGeneration(t, []generation.Resource{{
				Key: generation.ResourceKey{Kind: "routes", ID: "public-api-prometheus-route"},
				Value: []byte(`{
					"id":"public-api-prometheus-route",
					"uri":"/metrics",
					"methods":["GET"],
					"plugins":{"public-api":{"uri":"/internal/metrics"}},
					"upstream":{"nodes":{"127.0.0.1:1":1}}
				}`),
			}}, func(cfg *config.Config) {
				cfg.Plugins = []string{"prometheus", "public-api"}
				cfg.PluginAttr = map[string]map[string]any{
					"prometheus": {
						"enable_export_server": test.enableExporter,
						"export_uri":           "/internal/metrics",
					},
				}
			})
			response := httptest.NewRecorder()
			prepared.HTTP().Handler().ServeHTTP(
				response,
				httptest.NewRequest(http.MethodGet, "http://gateway.test/metrics", nil),
			)
			if response.Code != test.wantStatus {
				t.Fatalf(
					"metrics status = %d, want %d; body=%s",
					response.Code, test.wantStatus, response.Body.String(),
				)
			}
		})
	}
}

func TestPreparedGenerationRoutePublicAPIPrecedesPrometheusURI(t *testing.T) {
	prepared := compileRouteGeneration(t, []generation.Resource{
		{
			Key: generation.ResourceKey{
				Kind: "routes",
				ID:   "public-api-prometheus-collision-producer",
			},
			Value: []byte(`{
				"id":"public-api-prometheus-collision-producer",
				"uri":"/example-unused",
				"methods":["GET"],
				"plugins":{"example-plugin":{"i":1}},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
		{
			Key: generation.ResourceKey{
				Kind: "routes",
				ID:   "public-api-prometheus-collision-exposure",
			},
			Value: []byte(`{
				"id":"public-api-prometheus-collision-exposure",
				"uri":"/example-control",
				"methods":["GET"],
				"plugins":{"public-api":{"uri":"/v1/plugin/example-plugin/hello"}},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
	}, func(cfg *config.Config) {
		cfg.Plugins = []string{"example-plugin", "prometheus", "public-api"}
		cfg.PluginAttr = map[string]map[string]any{
			"prometheus": {
				"enable_export_server": false,
				"export_uri":           "/v1/plugin/example-plugin/hello",
			},
		}
	})
	response := httptest.NewRecorder()
	prepared.HTTP().Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/example-control", nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := response.Body.String(); got != "world\n" {
		t.Fatalf("body = %q, want example-plugin public API response", got)
	}
}

func TestPreparedGenerationRejectsDisabledNestedPlugins(t *testing.T) {
	for _, test := range []struct {
		name    string
		routeID string
		raw     string
		plugins []string
	}{
		{
			name: "multi-auth child", routeID: "allowlist-multi-auth",
			raw: `{
				"id":"allowlist-multi-auth","uri":"/allowlist-multi-auth",
				"plugins":{"multi-auth":{"auth_plugins":[{"basic-auth":{}},{"key-auth":{}}]}}
			}`,
			plugins: []string{"multi-auth", "basic-auth"},
		},
		{
			name: "workflow action", routeID: "allowlist-workflow",
			raw: `{
				"id":"allowlist-workflow","uri":"/allowlist-workflow",
				"plugins":{"workflow":{"rules":[{"actions":[["limit-req",{"rate":1,"burst":0,"key":"remote_addr"}]]}]}}
			}`,
			plugins: []string{"workflow"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := compileRouteGeneration(t, []generation.Resource{{
				Key:   generation.ResourceKey{Kind: "routes", ID: test.routeID},
				Value: []byte(test.raw),
			}}, func(cfg *config.Config) {
				cfg.Plugins = append([]string(nil), test.plugins...)
			})
			quarantined := prepared.HTTP().Quarantined()
			if len(quarantined) != 1 ||
				quarantined[0] != (generation.ResourceKey{Kind: "routes", ID: test.routeID}) {
				t.Fatalf("quarantined = %#v, want route %q", quarantined, test.routeID)
			}
		})
	}
}

func TestPreparedGenerationResolvesCasdoorSecretsBeforePluginValidation(t *testing.T) {
	t.Setenv("CAS_CURRENT", "route-current-route-current-route-current")
	t.Setenv("CAS_FALLBACK", "route-fallback-route-fallback-route-fallback")
	prepared := compileRouteGeneration(t, []generation.Resource{{
		Key: generation.ResourceKey{Kind: "routes", ID: "casdoor-short-references"},
		Value: []byte(`{
			"id":"casdoor-short-references","uri":"/casdoor-short-references",
			"plugins":{"authz-casdoor":{
				"endpoint_addr":"https://door.example.com",
				"client_id":"compiler-client",
				"client_secret":"$ENV://CAS_CURRENT",
				"client_secret_fallbacks":["$ENV://CAS_FALLBACK"],
				"callback_url":"https://gateway.example.com/callback"
			}}
		}`),
	}}, func(cfg *config.Config) {
		cfg.Plugins = []string{"authz-casdoor"}
	})
	response := httptest.NewRecorder()
	prepared.HTTP().Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/casdoor-short-references", nil),
	)
	if response.Code != http.StatusFound {
		t.Fatalf("Casdoor route status = %d, want authorization redirect", response.Code)
	}
	if response.Header().Get("Set-Cookie") == "" {
		t.Fatal("Casdoor route did not seal a session cookie with the resolved secret")
	}
}

func TestPreparedGenerationCasdoorResolvedLengthFailureIsQuarantinedAndRetryable(t *testing.T) {
	const (
		currentReference  = "$ENV://CAS_CURRENT"
		fallbackReference = "$ENV://CAS_FALLBACK"
		validCurrent      = "route-current-route-current-route-current"
		validFallback     = "route-fallback-route-fallback-route-fallback"
	)
	for _, test := range []struct {
		name     string
		shortEnv string
	}{
		{name: "current", shortEnv: "CAS_CURRENT"},
		{name: "fallback", shortEnv: "CAS_FALLBACK"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(backend.Close)
			t.Setenv("CAS_CURRENT", validCurrent)
			t.Setenv("CAS_FALLBACK", validFallback)
			t.Setenv(test.shortEnv, "short-private-value")
			resources := []generation.Resource{
				{
					Key: generation.ResourceKey{Kind: "routes", ID: "casdoor-short-" + test.name},
					Value: fmt.Appendf(nil, `{
						"id":"casdoor-short-%s","uri":"/casdoor-short-%s",
						"plugins":{"authz-casdoor":{
							"endpoint_addr":"https://door.example.com","client_id":"compiler-client",
							"client_secret":%q,"client_secret_fallbacks":[%q],
							"callback_url":"https://gateway.example.com/callback"
						}}
					}`, test.name, test.name, currentReference, fallbackReference),
				},
				{
					Key: generation.ResourceKey{Kind: "routes", ID: "casdoor-valid-sibling"},
					Value: fmt.Appendf(nil, `{
						"id":"casdoor-valid-sibling","uri":"/casdoor-valid-sibling",
						"upstream":{"nodes":{%q:1}}
					}`, strings.TrimPrefix(backend.URL, "http://")),
				},
			}
			configure := func(cfg *config.Config) { cfg.Plugins = []string{"authz-casdoor"} }
			harness := newRouteGenerationFactory(t, configure)
			prepared, err := harness.Prepare(t, resources, generation.DomainHTTP)
			if err != nil {
				t.Fatalf("invalid Casdoor route failed the whole generation: %v", err)
			}
			quarantined := prepared.HTTP().Quarantined()
			if len(quarantined) != 1 || quarantined[0].ID != "casdoor-short-"+test.name {
				t.Fatalf("quarantined = %#v, want failed Casdoor route", quarantined)
			}
			sibling := httptest.NewRecorder()
			prepared.HTTP().Handler().ServeHTTP(
				sibling,
				httptest.NewRequest(http.MethodGet, "http://gateway.test/casdoor-valid-sibling", nil),
			)
			if sibling.Code != http.StatusNoContent {
				t.Fatalf("valid sibling status = %d, want %d", sibling.Code, http.StatusNoContent)
			}

			t.Setenv(test.shortEnv, "retry-secret-retry-secret-retry-secret")
			retry, err := harness.Prepare(t, resources, generation.DomainHTTP)
			if err != nil {
				t.Fatalf("retry corrected Casdoor generation: %v", err)
			}
			if got := retry.HTTP().Quarantined(); len(got) != 0 {
				t.Fatalf("retry quarantine = %#v, want none", got)
			}
			response := httptest.NewRecorder()
			retry.HTTP().Handler().ServeHTTP(
				response,
				httptest.NewRequest(http.MethodGet, "http://gateway.test/casdoor-short-"+test.name, nil),
			)
			if response.Code != http.StatusFound {
				t.Fatalf("retry status = %d, want authorization redirect", response.Code)
			}
		})
	}
}

func TestPreparedGenerationCasdoorShortLiteralIsQuarantined(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	prepared, err := prepareRouteGeneration(t, []generation.Resource{
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "casdoor-short-literal"},
			Value: []byte(`{
				"id":"casdoor-short-literal","uri":"/casdoor-short-literal",
				"plugins":{"authz-casdoor":{
					"endpoint_addr":"https://door.example.com","client_id":"compiler-client",
					"client_secret":"short-literal-private",
					"callback_url":"https://gateway.example.com/callback"
				}}
			}`),
		},
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "casdoor-short-literal-sibling"},
			Value: fmt.Appendf(nil, `{
				"id":"casdoor-short-literal-sibling","uri":"/casdoor-short-literal-sibling",
				"upstream":{"nodes":{%q:1}}
			}`, strings.TrimPrefix(backend.URL, "http://")),
		},
	}, func(cfg *config.Config) { cfg.Plugins = []string{"authz-casdoor"} })
	if err != nil {
		t.Fatalf("short literal failed the whole generation: %v", err)
	}
	quarantined := prepared.HTTP().Quarantined()
	if len(quarantined) != 1 || quarantined[0].ID != "casdoor-short-literal" {
		t.Fatalf("quarantined = %#v, want short-literal Casdoor route", quarantined)
	}
	sibling := httptest.NewRecorder()
	prepared.HTTP().Handler().ServeHTTP(
		sibling,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/casdoor-short-literal-sibling", nil),
	)
	if sibling.Code != http.StatusNoContent {
		t.Fatalf("short-literal sibling status = %d, want %d", sibling.Code, http.StatusNoContent)
	}
}

func TestPreparedGenerationInheritsServiceHosts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	resources := []generation.Resource{
		{
			Key:   generation.ResourceKey{Kind: "services", ID: "route-service-hosts"},
			Value: []byte(`{"id":"route-service-hosts","hosts":["service.example.com"]}`),
		},
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "service-host-route"},
			Value: fmt.Appendf(
				nil,
				`{"id":"service-host-route","uri":"/service-host","service_id":"route-service-hosts","upstream":{"nodes":{%q:1}}}`,
				strings.TrimPrefix(backend.URL, "http://"),
			),
		},
	}
	prepared := compileRouteGeneration(t, resources, nil)
	for _, test := range []struct {
		host string
		want int
	}{
		{host: "service.example.com", want: http.StatusNoContent},
		{host: "other.example.com", want: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		prepared.HTTP().Handler().ServeHTTP(
			response,
			httptest.NewRequest(http.MethodGet, "http://"+test.host+"/service-host", nil),
		)
		if response.Code != test.want {
			t.Fatalf("host %q status = %d, want %d", test.host, response.Code, test.want)
		}
	}
}

func TestPreparedGenerationTrafficSplitSnapshotOwnsRuntimeAcrossGenerationOverlap(t *testing.T) {
	var firstHits atomic.Int32
	firstBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(firstBackend.Close)
	var secondHits atomic.Int32
	secondBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(secondBackend.Close)
	harness := newRouteGenerationFactory(t, func(cfg *config.Config) {
		cfg.Plugins = []string{"traffic-split"}
	})
	routeResource := generation.Resource{
		Key: generation.ResourceKey{Kind: "routes", ID: "traffic-split-generation-owner"},
		Value: []byte(`{
			"id":"traffic-split-generation-owner","uri":"/traffic-split-generation-owner",
			"plugins":{"traffic-split":{"rules":[{"weighted_upstreams":[{
				"weight":1,"upstream_id":"shared-traffic-split-upstream"
			}]}]}},
			"upstream":{"nodes":{"127.0.0.1:1":1}}
		}`),
	}
	first, err := harness.Prepare(t, []generation.Resource{
		routeResource,
		{
			Key: generation.ResourceKey{Kind: "upstreams", ID: "shared-traffic-split-upstream"},
			Value: fmt.Appendf(
				nil,
				`{"id":"shared-traffic-split-upstream","nodes":{%q:1}}`,
				strings.TrimPrefix(firstBackend.URL, "http://"),
			),
		},
	}, generation.DomainHTTP)
	if err != nil {
		t.Fatalf("prepare first traffic-split generation: %v", err)
	}
	second, err := harness.Prepare(t, []generation.Resource{
		routeResource,
		{
			Key: generation.ResourceKey{Kind: "upstreams", ID: "shared-traffic-split-upstream"},
			Value: fmt.Appendf(
				nil,
				`{"id":"shared-traffic-split-upstream","nodes":{%q:1}}`,
				strings.TrimPrefix(secondBackend.URL, "http://"),
			),
		},
	}, generation.DomainHTTP)
	if err != nil {
		t.Fatalf("prepare second traffic-split generation: %v", err)
	}
	firstResponse := httptest.NewRecorder()
	first.HTTP().Handler().ServeHTTP(
		firstResponse,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/traffic-split-generation-owner", nil),
	)
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("first generation status = %d, want %d", firstResponse.Code, http.StatusCreated)
	}
	if got := firstHits.Load(); got != 1 {
		t.Fatalf("first backend hits after first generation request = %d, want 1", got)
	}
	if got := secondHits.Load(); got != 0 {
		t.Fatalf("second backend hits after first generation request = %d, want 0", got)
	}
	secondResponse := httptest.NewRecorder()
	second.HTTP().Handler().ServeHTTP(
		secondResponse,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/traffic-split-generation-owner", nil),
	)
	if secondResponse.Code != http.StatusAccepted {
		t.Fatalf("second generation status = %d, want %d", secondResponse.Code, http.StatusAccepted)
	}
	if got := firstHits.Load(); got != 1 {
		t.Fatalf("first backend hits after second generation request = %d, want 1", got)
	}
	if got := secondHits.Load(); got != 1 {
		t.Fatalf("second backend hits after second generation request = %d, want 1", got)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("close first traffic-split generation: %v", err)
	}
	response := httptest.NewRecorder()
	second.HTTP().Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/traffic-split-generation-owner", nil),
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("second traffic-split generation after first close = %d, want %d", response.Code, http.StatusAccepted)
	}
	if got := firstHits.Load(); got != 1 {
		t.Fatalf("first backend hits after first generation close = %d, want 1", got)
	}
	if got := secondHits.Load(); got != 2 {
		t.Fatalf("second backend hits after first generation close = %d, want 2", got)
	}
}

func TestPreparedGenerationExplicitFalseWebsocketOverridesService(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	prepared := compileRouteGeneration(t, []generation.Resource{
		{
			Key: generation.ResourceKey{Kind: "services", ID: "route-websocket-inherit-service"},
			Value: fmt.Appendf(
				nil,
				`{"id":"route-websocket-inherit-service","enable_websocket":true,"upstream":{"nodes":{%q:1}}}`,
				strings.TrimPrefix(backend.URL, "http://"),
			),
		},
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "route-websocket-explicit-false"},
			Value: []byte(`{
				"id":"route-websocket-explicit-false","uri":"/websocket-explicit-false",
				"service_id":"route-websocket-inherit-service","enable_websocket":false
			}`),
		},
	}, nil)
	request := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.test/websocket-explicit-false",
		nil,
	)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	prepared.HTTP().Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf(
			"explicit false websocket status = %d, want %d",
			response.Code,
			http.StatusBadRequest,
		)
	}
}

func TestPreparedGenerationFailureDoesNotPolluteEarlierPublicAPIRegistry(t *testing.T) {
	harness := newRouteGenerationFactory(t, func(cfg *config.Config) {
		cfg.Plugins = []string{"example-plugin", "public-api", "wolf-rbac"}
	})
	firstWolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(
			`{"ok":true,"data":{"token":"public-api-registry-token","userInfo":{"username":"alice"}}}`,
		))
	}))
	t.Cleanup(firstWolf.Close)
	first, err := harness.Prepare(t, []generation.Resource{
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "public-api-registry-wolf-route"},
			Value: fmt.Appendf(nil, `{
				"id":"public-api-registry-wolf-route","uri":"/wolf-unused","methods":["GET"],"priority":1,
				"plugins":{"wolf-rbac":{"server":%q}},"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`, firstWolf.URL),
		},
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "public-api-registry-public-route"},
			Value: []byte(`{
				"id":"public-api-registry-public-route","uri":"/expose","methods":["POST"],"priority":2,
				"plugins":{"public-api":{"uri":"/apisix/plugin/wolf-rbac/login"}},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
		{
			Key: generation.ResourceKey{Kind: "consumers", ID: "public-api-registry-user"},
			Value: []byte(`{
				"username":"public-api-registry-user",
				"plugins":{"wolf-rbac":{"appid":"public-api-registry-app","header_prefix":"X-","ssl_verify":false}}
			}`),
		},
	}, generation.DomainHTTP)
	if err != nil {
		t.Fatalf("prepare first generation: %v", err)
	}

	failed, err := harness.Prepare(t, []generation.Resource{
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "failed-candidate-public-api-owner"},
			Value: []byte(`{
				"id":"failed-candidate-public-api-owner","uri":"/candidate-owner",
				"plugins":{"example-plugin":{"i":2}},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "failed-candidate-public-api-exposure"},
			Value: []byte(`{
				"id":"failed-candidate-public-api-exposure","uri":"/candidate-exposure",
				"plugins":{"public-api":{"uri":"/v1/plugin/example-plugin/hello"}},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "stream_routes", ID: "failed-after-http-public-api"},
			Value: []byte(`{"id":"failed-after-http-public-api"}`),
		},
	}, generation.DomainHTTP, generation.DomainStream)
	if failed != nil || err == nil {
		t.Fatalf("failed candidate = (%v, %v), want nil/error after late preparation failure", failed, err)
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"http://gateway.test/expose",
		strings.NewReader(
			`{"appid":"public-api-registry-app","username":"admin","password":"secret"}`,
		),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	first.HTTP().Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"first generation status = %d, want 200; body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var body map[string]any
	if err := apisixjson.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode first generation response: %v", err)
	}
	if got := body["rbac_token"]; got != "V1#public-api-registry-app#public-api-registry-token" {
		t.Fatalf("first generation token = %v, want first backend token", got)
	}
}

func TestPreparedGenerationQuarantineRollsBackPublicAPIRegistryMutations(t *testing.T) {
	t.Setenv("PUBLIC_API_ROLLBACK_TOKEN", "candidate-token")
	harness := newRouteGenerationFactory(t, func(cfg *config.Config) {
		cfg.Plugins = []string{"example-plugin", "lago", "public-api"}
	})
	first, err := harness.Prepare(t, []generation.Resource{
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "public-api-baseline-owner"},
			Value: []byte(`{
				"id":"public-api-baseline-owner","uri":"/baseline-owner",
				"plugins":{"example-plugin":{"i":1}},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "public-api-baseline-exposure"},
			Value: []byte(`{
				"id":"public-api-baseline-exposure","uri":"/baseline-example-exposure",
				"plugins":{"public-api":{"uri":"/v1/plugin/example-plugin/hello"}},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
	}, generation.DomainHTTP)
	if err != nil {
		t.Fatalf("prepare baseline generation: %v", err)
	}

	candidateResources := []generation.Resource{
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "public-api-quarantine-invalid"},
			Value: []byte(`{
				"id":"public-api-quarantine-invalid","uri":"/quarantine-invalid-public-api",
				"plugins":{
					"example-plugin":{"i":2},
					"lago":{
						"endpoint_addrs":["http://127.0.0.1:3000"],
						"token":"$ENV://PUBLIC_API_ROLLBACK_TOKEN",
						"event_transaction_id":"transaction-id",
						"event_subscription_id":"subscription-id",
						"event_code":"event-code"
					}
				},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
		{
			Key: generation.ResourceKey{Kind: "routes", ID: "public-api-quarantine-exposure"},
			Value: []byte(`{
				"id":"public-api-quarantine-exposure","uri":"/quarantine-example-exposure",
				"plugins":{"public-api":{"uri":"/v1/plugin/example-plugin/hello"}},
				"upstream":{"nodes":{"127.0.0.1:1":1}}
			}`),
		},
	}
	var candidateOwner resource.Route
	if err := apisixjson.Unmarshal(candidateResources[0].Value, &candidateOwner); err != nil {
		t.Fatalf("decode candidate owner route: %v", err)
	}
	plan, err := routepkg.PlanHTTPPlugins(context.Background(), routepkg.PlanningInput{
		Routes:         []resource.Route{candidateOwner},
		EnabledPlugins: []string{"example-plugin", "lago"},
	})
	if err != nil {
		t.Fatalf("plan candidate owner plugins: %v", err)
	}
	if len(plan.Routes) != 1 || len(plan.Routes[0].Local) != 2 {
		t.Fatalf("candidate owner plugin plan = %#v, want two local plugins", plan.Routes)
	}
	gotOrder := []string{
		plan.Routes[0].Local[0].Factory,
		plan.Routes[0].Local[1].Factory,
	}
	if gotOrder[0] != "example-plugin" || gotOrder[1] != "lago" {
		t.Fatalf("candidate owner plugin order = %v, want [example-plugin lago]", gotOrder)
	}

	registered, err := harness.Prepare(t, candidateResources, generation.DomainHTTP)
	if err != nil {
		t.Fatalf("prepare candidate with materializable Lago token: %v", err)
	}
	registeredExposure := httptest.NewRecorder()
	registered.HTTP().Handler().ServeHTTP(
		registeredExposure,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/quarantine-example-exposure", nil),
	)
	if registeredExposure.Code != http.StatusOK || registeredExposure.Body.String() != "world\n" {
		t.Fatalf(
			"registered candidate exposure = %d/%q, want 200/world",
			registeredExposure.Code,
			registeredExposure.Body.String(),
		)
	}

	t.Setenv("PUBLIC_API_ROLLBACK_TOKEN", "")
	prepared, err := harness.Prepare(t, candidateResources, generation.DomainHTTP)
	if err != nil {
		t.Fatalf("prepare quarantined generation: %v", err)
	}
	quarantined := prepared.HTTP().Quarantined()
	if len(quarantined) != 1 || quarantined[0].ID != "public-api-quarantine-invalid" {
		t.Fatalf("quarantined = %#v, want invalid route only", quarantined)
	}
	exposure := httptest.NewRecorder()
	prepared.HTTP().
		Handler().
		ServeHTTP(exposure, httptest.NewRequest(http.MethodGet, "http://gateway.test/quarantine-example-exposure", nil))
	if exposure.Code != http.StatusNotFound {
		t.Fatalf(
			"rolled-back public API exposure status = %d, want %d",
			exposure.Code,
			http.StatusNotFound,
		)
	}
	baseline := httptest.NewRecorder()
	first.HTTP().Handler().ServeHTTP(
		baseline,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/baseline-example-exposure", nil),
	)
	if baseline.Code != http.StatusOK || baseline.Body.String() != "world\n" {
		t.Fatalf("baseline generation response = %d/%q, want 200/world", baseline.Code, baseline.Body.String())
	}
}

func compileRouteGeneration(
	t *testing.T,
	resources []generation.Resource,
	configure func(*config.Config),
) *compiler.PreparedGeneration {
	t.Helper()
	prepared, err := prepareRouteGeneration(t, resources, configure)
	if err != nil {
		t.Fatalf("prepare route generation: %v", err)
	}
	return prepared
}

func prepareRouteGeneration(
	t *testing.T,
	resources []generation.Resource,
	configure func(*config.Config),
) (*compiler.PreparedGeneration, error) {
	t.Helper()
	return newRouteGenerationFactory(t, configure).Prepare(t, resources, generation.DomainHTTP)
}

type routeGenerationFactory struct {
	factory  *compiler.WorkerCompilerFactory
	resolver *secret.GenerationSecretResolver
	revision uint64
}

func newRouteGenerationFactory(
	t *testing.T,
	configure func(*config.Config),
) *routeGenerationFactory {
	t.Helper()
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatalf("build secret catalog: %v", err)
	}
	encryption := data_encryption.NewService(false, nil, catalog)
	resolver, err := secret.NewGenerationSecretResolver(encryption)
	if err != nil {
		t.Fatalf("build generation secret resolver: %v", err)
	}
	staticConfig := config.Config{}
	if configure != nil {
		configure(&staticConfig)
	}
	effective := &config.EffectiveConfig{Config: staticConfig}
	factory, err := compiler.NewWorkerCompilerFactory(
		effective,
		secret.NewMaterializer(encryption, resolver),
		compiler.WorkerRuntimeObservers{Cluster: proxy.NopClusterObserver{}},
	)
	if err != nil {
		_ = resolver.Close(context.Background())
		t.Fatalf("build compiler factory: %v", err)
	}
	harness := &routeGenerationFactory{factory: factory, resolver: resolver}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("WorkerCompilerFactory.Close() error = %v", err)
		}
		if err := resolver.Close(context.Background()); err != nil {
			t.Errorf("GenerationSecretResolver.Close() error = %v", err)
		}
	})
	return harness
}

func (harness *routeGenerationFactory) Prepare(
	t *testing.T,
	resources []generation.Resource,
	domains ...generation.Domain,
) (*compiler.PreparedGeneration, error) {
	t.Helper()
	if len(domains) == 0 {
		domains = []generation.Domain{generation.DomainHTTP}
	}
	harness.revision++
	desired, err := generation.NewSnapshot(harness.revision, resources, nil)
	if err != nil {
		t.Fatalf("build generation snapshot: %v", err)
	}
	prepared, err := harness.factory.PrepareGeneration(
		context.Background(),
		generation.ApplyTicket{
			DesiredRevision: harness.revision,
			DesiredDigest:   desired.Digest(),
			Cursor: generation.ProviderCursor{
				Provider: "route-test", Revision: fmt.Sprint(harness.revision),
			},
			RequiredDomains: domains,
		},
		desired,
		nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := prepared.Close(context.Background()); err != nil {
			t.Errorf("PreparedGeneration.Close() error = %v", err)
		}
	})
	return prepared, nil
}
