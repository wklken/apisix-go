package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestPublicAPIExposesBatchRequestsAtCustomRoute(t *testing.T) {
	registry := public_api.NewRegistry()

	mux := chi.NewRouter()
	mux.Get("/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello " + r.URL.Query().Get("name")))
	})
	registerExtraRoutes(mux, &config.Config{Plugins: []string{"batch-requests"}}, registry)

	p := newPublicAPITestPlugin(t, map[string]any{"uri": "/apisix/batch-requests"}, registry)
	mux.Method(http.MethodPost, "/batch", p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("public-api should dispatch to internal public API without calling next")
	})))

	req := httptest.NewRequest(http.MethodPost, "/batch", strings.NewReader(`{
		"pipeline": [
			{"method": "GET", "path": "/hello", "query": {"name": "alice"}}
		]
	}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()

	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	var body []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("response len = %d, want 1", len(body))
	}
	if body[0]["status"] != float64(http.StatusOK) {
		t.Fatalf("subresponse status = %v, want 200", body[0]["status"])
	}
	if body[0]["body"] != "hello alice" {
		t.Fatalf("subresponse body = %q, want hello alice", body[0]["body"])
	}
}

func TestPublicAPIUsesRouteURIWhenConfigURIEmpty(t *testing.T) {
	registry := public_api.NewRegistry()

	mux := chi.NewRouter()
	registerExtraRoutes(mux, &config.Config{Plugins: []string{"node-status"}}, registry)
	p := newPublicAPITestPlugin(t, map[string]any{}, registry)
	mux.Method(
		http.MethodGet,
		"/apisix/status",
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("public-api should dispatch to internal public API without calling next")
		})),
	)

	req := httptest.NewRequest(http.MethodGet, "/apisix/status", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["id"] == "" {
		t.Fatalf("id = %v, want non-empty", body["id"])
	}
}

func TestPublicAPIExposesConfiguredPrometheusEndpointPerGeneration(t *testing.T) {
	ensureRouteStore(t)
	putRouteResource(t, "public-api-prometheus-route", []byte(
		`{"id":"public-api-prometheus-route","uri":"/metrics","methods":["GET"],"plugins":{"public-api":{"uri":"/internal/metrics"}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
	))

	for _, test := range []struct {
		name           string
		enableExporter bool
		wantStatus     int
	}{
		{name: "dedicated exporter disabled", enableExporter: false, wantStatus: http.StatusOK},
		{name: "dedicated exporter enabled", enableExporter: true, wantStatus: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			effective := testEffectiveConfig()
			effective.Config.Plugins = []string{"prometheus", "public-api"}
			effective.Config.PluginAttr = map[string]map[string]any{
				"prometheus": {
					"enable_export_server": test.enableExporter,
					"export_uri":           "/internal/metrics",
				},
			}
			builder := NewBuilder(nil, effective, testDataEncryptionResolver())
			t.Cleanup(builder.Stop)
			mux, err := builder.BuildStrict()
			if err != nil {
				t.Fatalf("BuildStrict() error = %v", err)
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if response.Code != test.wantStatus {
				t.Fatalf(
					"metrics status = %d, want %d; body=%s",
					response.Code,
					test.wantStatus,
					response.Body.String(),
				)
			}
		})
	}
}

func TestRoutePluginPublicAPIKeepsPrecedenceOverPrometheusURI(t *testing.T) {
	effective := testEffectiveConfig()
	effective.Config.Plugins = []string{"example-plugin", "prometheus", "public-api"}
	effective.Config.PluginAttr = map[string]map[string]any{
		"prometheus": {
			"enable_export_server": false,
			"export_uri":           "/v1/plugin/example-plugin/hello",
		},
	}
	ensureRouteStore(t)
	putRouteResource(t, "public-api-prometheus-collision-producer", []byte(
		`{"id":"public-api-prometheus-collision-producer","uri":"/example-unused","methods":["GET"],"plugins":{"example-plugin":{"i":1}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
	))
	putRouteResource(t, "public-api-prometheus-collision-exposure", []byte(
		`{"id":"public-api-prometheus-collision-exposure","uri":"/example-control","methods":["GET"],"plugins":{"public-api":{"uri":"/v1/plugin/example-plugin/hello"}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
	))

	builder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	mux, err := builder.BuildStrict()
	if err != nil {
		t.Fatalf("BuildStrict() error = %v", err)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/example-control", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := response.Body.String(); got != "world\n" {
		t.Fatalf("body = %q, want example-plugin public API response", got)
	}
}

func TestPublicAPIReturnsNotFoundForUnknownInternalURI(t *testing.T) {
	registry := public_api.NewRegistry()

	p := newPublicAPITestPlugin(t, map[string]any{"uri": "/missing"}, registry)
	req := httptest.NewRequest(http.MethodGet, "/expose", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("public-api should not call next when no internal API matches")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("response code = %d, want 404", res.Code)
	}
}

func TestFailedBuildDoesNotPolluteEarlierPublicAPIRegistry(t *testing.T) {
	effective := testEffectiveConfig()
	effective.Config.Plugins = []string{"public-api", "wolf-rbac"}
	firstWolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(
			[]byte(`{"ok":true,"data":{"token":"public-api-registry-token","userInfo":{"username":"alice"}}}`),
		)
	}))
	t.Cleanup(firstWolf.Close)
	secondWolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"token":"second-generation-token","userInfo":{"username":"bob"}}}`))
	}))
	t.Cleanup(secondWolf.Close)
	ensureRouteStore(t)
	putRouteResource(t, "public-api-registry-wolf-route", fmt.Appendf(
		nil,
		`{"id":"public-api-registry-wolf-route","uri":"/wolf-unused","methods":["GET"],"priority":1,"plugins":{"wolf-rbac":{"server":%q}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
		firstWolf.URL,
	))
	putRouteResource(t, "public-api-registry-public-route", []byte(
		`{"id":"public-api-registry-public-route","uri":"/expose","methods":["POST"],"priority":2,"plugins":{"public-api":{"uri":"/apisix/plugin/wolf-rbac/login"}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
	))
	putRouteConsumerForPublicAPI(t)

	firstBuilder := NewBuilder(nil, effective, testDataEncryptionResolver())
	firstMux, err := firstBuilder.BuildStrict()
	if err != nil {
		t.Fatalf("first BuildStrict() error = %v", err)
	}
	t.Cleanup(firstBuilder.Stop)

	putRouteResource(t, "public-api-registry-wolf-route", fmt.Appendf(
		nil,
		`{"id":"public-api-registry-wolf-route","uri":"/wolf-unused","methods":["GET"],"priority":1,"plugins":{"wolf-rbac":{"server":%q}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
		secondWolf.URL,
	))
	putRouteResource(t, "public-api-registry-failing-route", []byte(
		`{"id":"public-api-registry-failing-route","uri":"/bad{uri}","methods":["GET"],"priority":10,"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
	))
	secondBuilder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(secondBuilder.Stop)
	if secondMux, buildErr := secondBuilder.BuildStrict(); buildErr == nil || secondMux != nil {
		t.Fatalf("second BuildStrict() = (%T, %v), want failed build", secondMux, buildErr)
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"/expose",
		strings.NewReader(`{"appid":"public-api-registry-app","username":"admin","password":"secret"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	firstMux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("first mux status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode first mux response: %v", err)
	}
	if got := body["rbac_token"]; got != "V1#public-api-registry-app#public-api-registry-token" {
		t.Fatalf("first mux token = %v, want first generation backend token", got)
	}
}

func TestQuarantinedRouteRollsBackPublicAPIRegistryMutations(t *testing.T) {
	effective := testEffectiveConfig()
	effective.Config.Plugins = []string{"example-plugin", "public-api", "wolf-rbac", "workflow"}
	ensureRouteStore(t)
	putRouteResource(t, "public-api-quarantine-invalid", []byte(
		`{"id":"public-api-quarantine-invalid","uri":"/quarantine-invalid-public-api","methods":["GET"],"priority":1,"plugins":{"example-plugin":{"i":1},"wolf-rbac":{"server":"http://127.0.0.1:19101"},"workflow":{"rules":[{"case":[["uri","bogus","/bad"]],"actions":[["return",{"code":200}]]}]}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
	))
	putRouteResource(t, "public-api-quarantine-valid", []byte(
		`{"id":"public-api-quarantine-valid","uri":"/quarantine-valid-public-api","methods":["GET"],"priority":2,"plugins":{"wolf-rbac":{"server":"http://127.0.0.1:19100"}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
	))
	putRouteResource(t, "public-api-quarantine-exposure", []byte(
		`{"id":"public-api-quarantine-exposure","uri":"/quarantine-example-exposure","methods":["GET"],"priority":3,"plugins":{"public-api":{"uri":"/v1/plugin/example-plugin/hello"}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
	))

	builder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildWithRouteQuarantine()
	if err != nil || handler == nil {
		t.Fatalf("BuildWithRouteQuarantine() = (%T, %v), want published generation", handler, err)
	}
	if got, want := builder.QuarantinedResourceCount(), 1; got != want {
		t.Fatalf("QuarantinedResourceCount() = %d, want %d", got, want)
	}

	validRoute := httptest.NewRecorder()
	handler.ServeHTTP(validRoute, httptest.NewRequest(http.MethodGet, "/quarantine-valid-public-api", nil))
	if got, want := validRoute.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("successful route status = %d, want %d", got, want)
	}

	quarantinedExposure := httptest.NewRecorder()
	handler.ServeHTTP(
		quarantinedExposure,
		httptest.NewRequest(http.MethodGet, "/quarantine-example-exposure", nil),
	)
	if got, want := quarantinedExposure.Code, http.StatusNotFound; got != want {
		t.Fatalf("rolled-back public API exposure status = %d, want %d", got, want)
	}
}

func putRouteConsumerForPublicAPI(t *testing.T) {
	t.Helper()
	consumer := map[string]any{
		"username": "public-api-registry-user",
		"plugins": map[string]any{
			"wolf-rbac": map[string]any{
				"appid":         "public-api-registry-app",
				"header_prefix": "X-",
				"ssl_verify":    false,
			},
		},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal public API consumer: %v", err)
	}
	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/public-api-registry-user")
	event.Value = body
	routeStoreEvents <- event
	if err := routeStore.Sync(); err != nil {
		t.Fatalf("sync public API consumer: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumerByPluginKey("wolf-rbac", "public-api-registry-app"); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("public API consumer was not indexed for wolf-rbac")
}

func newPublicAPITestPlugin(
	t *testing.T,
	cfg map[string]any,
	registries ...*public_api.Registry,
) *public_api.Plugin {
	t.Helper()

	p := &public_api.Plugin{}
	registry := public_api.NewRegistry()
	if len(registries) > 0 && registries[0] != nil {
		registry = registries[0]
	}
	p.SetPublicAPIRegistry(registry)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(cfg, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}
