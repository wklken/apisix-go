package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
)

func TestHTTPPluginAllowlist(t *testing.T) {
	ensureRouteStore(t)
	tests := []struct {
		name       string
		bucket     string
		resourceID string
		resource   string
		route      string
		wantKind   string
	}{
		{
			name:       "route",
			resourceID: "allowlist-route",
			route:      `{"id":"allowlist-route","uri":"/allowlist-route","plugins":{"request-id":{}}}`,
			wantKind:   "route",
		},
		{
			name:       "plugin-config",
			bucket:     "plugin_configs",
			resourceID: "allowlist-plugin-config",
			resource:   `{"id":"allowlist-plugin-config","plugins":{"request-id":{}}}`,
			route:      `{"id":"allowlist-plugin-config-route","uri":"/allowlist-plugin-config","plugin_config_id":"allowlist-plugin-config"}`,
			wantKind:   "plugin_config",
		},
		{
			name:       "plugin-config-overridden-by-route",
			bucket:     "plugin_configs",
			resourceID: "allowlist-plugin-config-overridden",
			resource:   `{"id":"allowlist-plugin-config-overridden","plugins":{"request-id":{}}}`,
			route:      `{"id":"allowlist-override-route-pc","uri":"/allowlist-plugin-config-overridden","plugins":{"request-id":{}},"plugin_config_id":"allowlist-plugin-config-overridden"}`,
			wantKind:   "plugin_config",
		},
		{
			name:       "service",
			bucket:     "services",
			resourceID: "allowlist-service",
			resource:   `{"id":"allowlist-service","plugins":{"request-id":{}}}`,
			route:      `{"id":"allowlist-service-route","uri":"/allowlist-service","service_id":"allowlist-service"}`,
			wantKind:   "service",
		},
		{
			name:       "service-overridden-by-route",
			bucket:     "services",
			resourceID: "allowlist-service-overridden",
			resource:   `{"id":"allowlist-service-overridden","plugins":{"request-id":{}}}`,
			route:      `{"id":"allowlist-override-route-svc","uri":"/allowlist-service-overridden","plugins":{"request-id":{}},"service_id":"allowlist-service-overridden"}`,
			wantKind:   "service",
		},
		{
			name:       "global-rule",
			bucket:     "global_rules",
			resourceID: "allowlist-global-rule",
			resource:   `{"id":"allowlist-global-rule","plugins":{"request-id":{}}}`,
			route:      `{"id":"allowlist-global-route","uri":"/allowlist-global"}`,
			wantKind:   "global_rule",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setHTTPPluginAllowlist(t)
			if test.bucket != "" {
				putHTTPAllowlistResource(t, test.bucket, test.resourceID, []byte(test.resource))
			}
			putRouteResource(t, routeIDFromJSON(t, test.route), []byte(test.route))

			builder := NewBuilder(nil)
			t.Cleanup(builder.Stop)
			handler, err := builder.BuildStrict()
			if err == nil {
				t.Fatal("BuildStrict() error = nil, want disabled-plugin rejection")
			}
			if handler != nil {
				t.Fatalf("BuildStrict() handler = %T, want nil", handler)
			}
			for _, want := range []string{"request-id", test.wantKind, test.resourceID} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("BuildStrict() error = %q, want %q", err, want)
				}
			}
		})
	}

	t.Run("enabled controls build", func(t *testing.T) {
		setHTTPPluginAllowlist(t, "request-id")
		const id = "allowlist-enabled-route"
		putRouteResource(t, id, []byte(
			`{"id":"allowlist-enabled-route","uri":"/allowlist-enabled","plugins":{"request-id":{}}}`,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err != nil || handler == nil {
			t.Fatalf("BuildStrict() = (%T, %v), want enabled route handler", handler, err)
		}
	})

	t.Run("strict empty still builds system request context", func(t *testing.T) {
		setHTTPPluginAllowlist(t)
		const id = "allowlist-empty-route"
		putRouteResource(t, id, []byte(`{"id":"allowlist-empty-route","uri":"/allowlist-empty"}`))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err != nil || handler == nil {
			t.Fatalf("BuildStrict() = (%T, %v), want system request-context bypass", handler, err)
		}
	})

	t.Run("user request context requires membership", func(t *testing.T) {
		setHTTPPluginAllowlist(t)
		const id = "allowlist-user-request-context"
		putRouteResource(t, id, []byte(
			`{"id":"allowlist-user-request-context","uri":"/allowlist-user-request-context","plugins":{"request-context":{}}}`,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err == nil || handler != nil {
			t.Fatalf("BuildStrict() = (%T, %v), want user request-context rejection", handler, err)
		}
		if !strings.Contains(err.Error(), "request-context") {
			t.Fatalf("BuildStrict() error = %q, want request-context", err)
		}
	})

	t.Run("global body limit does not require client control membership", func(t *testing.T) {
		setHTTPPluginAllowlist(t)
		previous := appconfig.GlobalConfig
		appconfig.GlobalConfig = &appconfig.Config{NginxConfig: appconfig.NginxConfig{
			HTTP: appconfig.NginxHTTP{ClientMaxBodySize: 1},
		}}
		t.Cleanup(func() { appconfig.GlobalConfig = previous })
		const id = "allowlist-generated-client-control"
		putRouteResource(t, id, []byte(
			`{"id":"allowlist-generated-client-control","uri":"/allowlist-generated-client-control"}`,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err != nil || handler == nil {
			t.Fatalf("BuildStrict() = (%T, %v), want server-owned limit without generated client-control", handler, err)
		}
	})

	t.Run("metadata for an unmaterialized plugin is inert", func(t *testing.T) {
		setHTTPPluginAllowlist(t)
		putHTTPAllowlistResource(t, "plugin_metadata", "cors", []byte(`{"allow_origins":123}`))
		const id = "allowlist-inert-metadata"
		putRouteResource(t, id, []byte(`{"id":"allowlist-inert-metadata","uri":"/allowlist-inert-metadata"}`))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err != nil || handler == nil {
			t.Fatalf("BuildStrict() = (%T, %v), want inert metadata ignored", handler, err)
		}
	})

	t.Run("metadata disable does not bypass membership", func(t *testing.T) {
		setHTTPPluginAllowlist(t)
		const id = "allowlist-meta-disabled"
		putRouteResource(t, id, []byte(
			`{"id":"allowlist-meta-disabled","uri":"/allowlist-meta-disabled","plugins":{"request-id":{"_meta":{"disable":true}}}}`,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err == nil || handler != nil {
			t.Fatalf("BuildStrict() = (%T, %v), want disabled membership rejection before metadata", handler, err)
		}
		if !strings.Contains(err.Error(), "request-id") {
			t.Fatalf("BuildStrict() error = %q, want request-id", err)
		}
	})

	t.Run("nested multi-auth checker is injected", func(t *testing.T) {
		setHTTPPluginAllowlist(t, "multi-auth", "basic-auth")
		const id = "allowlist-multi-auth"
		putRouteResource(
			t,
			id,
			[]byte(
				`{"id":"allowlist-multi-auth","uri":"/allowlist-multi-auth","plugins":{"multi-auth":{"auth_plugins":[{"basic-auth":{}},{"key-auth":{}}]}}}`,
			),
		)

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err == nil || handler != nil {
			t.Fatalf("BuildStrict() = (%T, %v), want disabled nested auth rejection", handler, err)
		}
		if !strings.Contains(err.Error(), "key-auth") {
			t.Fatalf("BuildStrict() error = %q, want key-auth", err)
		}
	})

	t.Run("nested workflow checker is injected", func(t *testing.T) {
		setHTTPPluginAllowlist(t, "workflow")
		const id = "allowlist-workflow"
		putRouteResource(
			t,
			id,
			[]byte(
				`{"id":"allowlist-workflow","uri":"/allowlist-workflow","plugins":{"workflow":{"rules":[{"actions":[["limit-req",{"rate":1,"burst":0,"key":"remote_addr"}]]}]}}}`,
			),
		)

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err == nil || handler != nil {
			t.Fatalf("BuildStrict() = (%T, %v), want disabled nested workflow rejection", handler, err)
		}
		if !strings.Contains(err.Error(), "limit-req") {
			t.Fatalf("BuildStrict() error = %q, want limit-req", err)
		}
	})
}

func TestDisabledMCPBridge(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	marker := t.TempDir() + "/mcp-started"
	const id = "allowlist-disabled-mcp"
	routeValue := fmt.Appendf(
		nil,
		`{"id":%q,"uri":"/sse","plugins":{"mcp-bridge":{"command":"/bin/sh","args":["-c","printf started > %s"]}}}`,
		id,
		marker,
	)
	putRouteResource(
		t,
		id,
		routeValue,
	)

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err == nil {
		t.Fatal("BuildStrict() error = nil, want disabled mcp-bridge rejection")
	}
	if handler != nil {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sse", nil))
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("mcp marker stat error = %v, command may have started", statErr)
	}
}

func TestHTTPPluginAllowlistConsumerPlugin(t *testing.T) {
	t.Run("disabled consumer plugin fails closed", func(t *testing.T) {
		setHTTPPluginAllowlist(t)
		builder := buildAllowlistTestBuilder(t)
		marker := t.TempDir() + "/consumer-mcp-started"
		consumer := resource.Consumer{
			Username: "allowlist-consumer",
			Plugins: map[string]resource.PluginConfig{
				"mcp-bridge": map[string]any{
					"command": "/bin/sh",
					"args":    []any{"-c", "printf started > " + marker},
				},
			},
		}
		request := authenticatedAllowlistRequest(consumer)
		response := httptest.NewRecorder()
		var nextCalls atomic.Int32
		serveConsumerResolution(builder, response, request, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			nextCalls.Add(1)
		}), builder.pluginRouteContext(resource.Route{ID: "allowlist-consumer-route"}))

		if response.Code != http.StatusInternalServerError {
			t.Fatalf("consumer response code = %d, want 500", response.Code)
		}
		if nextCalls.Load() != 0 {
			t.Fatalf("consumer next calls = %d, want none", nextCalls.Load())
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("consumer mcp marker stat error = %v, command may have started", statErr)
		}
	})

	t.Run("disabled group plugin fails closed", func(t *testing.T) {
		setHTTPPluginAllowlist(t)
		const groupID = "allowlist-consumer-group"
		putHTTPAllowlistResource(
			t,
			"consumer_groups",
			groupID,
			[]byte(`{"id":"allowlist-consumer-group","plugins":{"mcp-bridge":{"command":"/bin/true"}}}`),
		)
		builder := buildAllowlistTestBuilder(t)
		consumer := resource.Consumer{Username: "allowlist-group-consumer", GroupID: groupID}
		request := authenticatedAllowlistRequest(consumer)
		response := httptest.NewRecorder()
		serveConsumerResolution(
			builder,
			response,
			request,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}),
			builder.pluginRouteContext(resource.Route{ID: "allowlist-group-route"}),
		)

		if response.Code != http.StatusInternalServerError {
			t.Fatalf("group response code = %d, want 500", response.Code)
		}
	})

	t.Run("enabled consumer plugin runs after config mutation", func(t *testing.T) {
		setHTTPPluginAllowlist(t, "limit-count")
		builder := buildAllowlistTestBuilder(t)
		if appconfig.GlobalConfig != nil {
			appconfig.GlobalConfig.Plugins = nil
		}
		consumer := resource.Consumer{
			Username: "allowlist-enabled-consumer",
			Plugins: map[string]resource.PluginConfig{
				"limit-count": map[string]any{
					"count":       1,
					"time_window": 60,
					"key":         "remote_addr",
				},
			},
		}
		request := authenticatedAllowlistRequest(consumer)
		response := httptest.NewRecorder()
		var nextCalls atomic.Int32
		serveConsumerResolution(
			builder,
			response,
			request,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nextCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}),
			builder.pluginRouteContext(resource.Route{ID: "allowlist-enabled-consumer-route"}),
		)

		if response.Code != http.StatusNoContent {
			t.Fatalf("enabled consumer response code = %d, want 204", response.Code)
		}
		if nextCalls.Load() != 1 {
			t.Fatalf("enabled consumer next calls = %d, want 1", nextCalls.Load())
		}
	})
}

func setHTTPPluginAllowlist(t *testing.T, names ...string) {
	t.Helper()
	previous := appconfig.GlobalConfig
	copyNames := append([]string(nil), names...)
	appconfig.GlobalConfig = &appconfig.Config{Plugins: copyNames}
	t.Cleanup(func() { appconfig.GlobalConfig = previous })
}

func putHTTPAllowlistResource(t *testing.T, bucket, id string, value []byte) {
	t.Helper()
	ensureRouteStore(t)
	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/" + bucket + "/" + id)
	event.Value = value
	routeStoreEvents <- event
	if err := routeStore.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	t.Cleanup(func() {
		remove := store.NewEvent()
		remove.Type = store.EventTypeDelete
		remove.Key = []byte("/apisix/" + bucket + "/" + id)
		routeStoreEvents <- remove
		if err := routeStore.Sync(); err != nil {
			t.Errorf("cleanup Sync() error = %v", err)
		}
	})
}

func routeIDFromJSON(t *testing.T, value string) string {
	t.Helper()
	var route resource.Route
	if err := apisixjson.Unmarshal([]byte(value), &route); err != nil {
		t.Fatalf("decode route fixture: %v", err)
	}
	return route.ID
}

func buildAllowlistTestBuilder(t *testing.T) *Builder {
	t.Helper()
	const id = "allowlist-consumer-base-route"
	putRouteResource(t, id, []byte(`{"id":"allowlist-consumer-base-route","uri":"/allowlist-consumer-base"}`))
	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	if handler, err := builder.BuildStrict(); err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want base handler", handler, err)
	}
	return builder
}

func authenticatedAllowlistRequest(consumer resource.Consumer) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/allowlist", nil)
	request = apisixctx.WithApisixVars(request, nil)
	apisixctx.AttachConsumer(request, consumer)
	return apisixctx.WithAuthenticationState(request, apisixctx.NewAuthenticationState("allowlist-test", consumer))
}

func serveConsumerResolution(
	builder *Builder,
	response http.ResponseWriter,
	request *http.Request,
	next http.Handler,
	routeContext pluginRouteContext,
) {
	pipeline := plugin.NewRequestPipeline(nil, builder.resolveConsumerBindings(routeContext))
	pipeline.Then(next).ServeHTTP(response, request)
}
