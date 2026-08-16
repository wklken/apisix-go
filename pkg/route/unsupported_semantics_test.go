package route

import (
	"os"
	"strings"
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestBuildStrictRejectsUnsupportedRouteSemantics(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)

	tests := []struct {
		name    string
		field   string
		value   string
		wantErr string
		routeID string
	}{
		{
			name:    "script",
			field:   "script",
			value:   `"return true"`,
			wantErr: "script",
			routeID: "unsupported-script-route",
		},
		{
			name:    "filter_func",
			field:   "filter_func",
			value:   `"return true"`,
			wantErr: "filter_func",
			routeID: "unsupported-filter-route",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(
				`{"id":"` + test.routeID + `","uri":"/` + test.routeID + `","` +
					test.field + `":` + test.value + `}`,
			)
			putRouteResource(t, test.routeID, payload)

			builder := NewBuilder(nil)
			t.Cleanup(builder.Stop)
			handler, err := builder.BuildStrict()
			if err == nil || handler != nil {
				t.Fatalf(
					"BuildStrict() = (%T, %v), want nil handler and unsupported %s error",
					handler,
					err,
					test.field,
				)
			}
			if !strings.Contains(err.Error(), test.routeID) ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("BuildStrict() error = %q, want route ID %q and field %q", err, test.routeID, test.wantErr)
			}
		})
	}
}

func TestBuildStrictAllowsBlankFilterFunc(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	const routeID = "blank-filter-route"
	putRouteResource(t, routeID, []byte(`{"id":"blank-filter-route","uri":"/blank-filter-route","filter_func":" \t "}`))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want blank filter_func accepted", handler, err)
	}
}

func TestBuildReverseHandlerValidatesHTTPUpstreamTypes(t *testing.T) {
	for _, test := range []struct {
		name   string
		scheme string
		type_  string
		wantOK bool
	}{
		{name: "default scheme empty type", type_: "", wantOK: true},
		{name: "http roundrobin", scheme: "http", type_: "roundrobin", wantOK: true},
		{name: "https empty type", scheme: "https", type_: "", wantOK: true},
		{name: "grpc roundrobin", scheme: "grpc", type_: "roundrobin", wantOK: true},
		{name: "grpcs empty type", scheme: "grpcs", type_: "", wantOK: true},
		{name: "http chash", scheme: "http", type_: "chash"},
		{name: "http random", scheme: "http", type_: "random"},
		{name: "kafka owner", scheme: "kafka", type_: "chash", wantOK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := &Builder{}
			t.Cleanup(builder.Stop)
			_, err := builder.buildReverseHandler(resource.Route{Upstream: resource.Upstream{
				Scheme: test.scheme,
				Type:   test.type_,
			}}, resource.Service{})
			if test.wantOK {
				if err != nil {
					t.Fatalf("buildReverseHandler() error = %v, want accepted upstream type", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "unsupported upstream type") {
				t.Fatalf("buildReverseHandler() error = %v, want unsupported upstream type", err)
			}
		})
	}
}

func TestBuildReverseHandlerRejectsKafkaInHTTPDataPlaneProfile(t *testing.T) {
	previous := appconfig.GlobalConfig
	t.Cleanup(func() { appconfig.GlobalConfig = previous })
	appconfig.GlobalConfig = &appconfig.Config{
		Deployment: appconfig.Deployment{Profile: appconfig.HTTPDataPlaneV1Profile},
	}

	builder := &Builder{}
	t.Cleanup(builder.Stop)
	_, err := builder.buildReverseHandler(resource.Route{
		ID:       "profile-kafka-route",
		Upstream: resource.Upstream{Scheme: "kafka", Type: "chash"},
	}, resource.Service{})
	if err == nil {
		t.Fatal("buildReverseHandler() error = nil, want profile to reject kafka upstream")
	}
	for _, want := range []string{
		appconfig.HTTPDataPlaneV1Profile,
		"kafka",
		"unsupported",
	} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Fatalf("buildReverseHandler() error = %q, want %q", err, want)
		}
	}
}

func TestDefaultConfigOmitsPlaceholderAIPlugin(t *testing.T) {
	data, err := os.ReadFile("../../conf/config-default.yaml")
	if err != nil {
		t.Fatalf("ReadFile(config-default.yaml) error = %v", err)
	}
	if strings.Contains(string(data), "- ai ") {
		t.Fatal("default config still enables the unsupported ai plugin")
	}
}
