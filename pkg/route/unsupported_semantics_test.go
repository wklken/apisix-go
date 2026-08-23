package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestBuildStrictRejectsUnsupportedRouteSemantics(t *testing.T) {
	ensureRouteStore(t)
	effective := httpPluginAllowlist()

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
		{
			name:    "vars",
			field:   "vars",
			value:   `[["http_user","==","ios"]]`,
			wantErr: "vars",
			routeID: "unsupported-vars-route",
		},
		{
			name:    "remote_addrs",
			field:   "remote_addrs",
			value:   `["10.0.0.1"]`,
			wantErr: "remote_addrs",
			routeID: "unsupported-remote-addrs-route",
		},
		{
			name:    "remote_addr",
			field:   "remote_addr",
			value:   `"10.0.0.1"`,
			wantErr: "remote_addr",
			routeID: "unsupported-remote-addr-route",
		},
		{
			name:    "script_id",
			field:   "script_id",
			value:   `"script-1"`,
			wantErr: "script_id",
			routeID: "unsupported-script-id-route",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(
				`{"id":"` + test.routeID + `","uri":"/` + test.routeID + `","` +
					test.field + `":` + test.value + `}`,
			)
			putRouteResource(t, test.routeID, payload)

			builder := NewBuilder(nil, effective, testDataEncryptionResolver())
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

func TestProgrammaticSingularFieldsUseValuePresence(t *testing.T) {
	hostRoute := resource.Route{ID: "programmatic-host-route", Host: "api.example.com"}
	if !hostRoute.HostConfigured() {
		t.Fatal("HostConfigured() = false, want non-empty programmatic host to be configured")
	}
	if hosts := hostRoute.EffectiveHosts(); len(hosts) != 1 || hosts[0] != hostRoute.Host {
		t.Fatalf("EffectiveHosts() = %#v, want [%q]", hosts, hostRoute.Host)
	}
	if err := validateRouteCompatibility(hostRoute); err != nil {
		t.Fatalf("validateRouteCompatibility() error = %v, want programmatic host accepted", err)
	}

	remoteRoute := resource.Route{ID: "programmatic-remote-addr-route", RemoteAddr: "10.0.0.1"}
	if !remoteRoute.RemoteAddrConfigured() {
		t.Fatal("RemoteAddrConfigured() = false, want non-empty programmatic remote_addr to be configured")
	}
	err := validateRouteCompatibility(remoteRoute)
	if err == nil {
		t.Fatal("validateRouteCompatibility() error = nil, want programmatic remote_addr rejection")
	}
	if !strings.Contains(err.Error(), remoteRoute.ID) || !strings.Contains(err.Error(), "remote_addr") {
		t.Fatalf("validateRouteCompatibility() error = %q, want route ID %q and field remote_addr", err, remoteRoute.ID)
	}
}

func TestBuildStrictAllowsBlankFilterFunc(t *testing.T) {
	ensureRouteStore(t)
	effective := httpPluginAllowlist()
	const routeID = "blank-filter-route"
	putRouteResource(t, routeID, []byte(`{"id":"blank-filter-route","uri":"/blank-filter-route","filter_func":" \t "}`))

	builder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want blank filter_func accepted", handler, err)
	}
}

func TestBuildStrictRejectsNestedNumericVars(t *testing.T) {
	ensureRouteStore(t)
	effective := httpPluginAllowlist()
	const routeID = "nested-numeric-vars"
	putRouteResource(
		t,
		routeID,
		[]byte(`{"id":"nested-numeric-vars","uri":"/nested-numeric-vars","vars":[["arg_age","==",18]]}`),
	)

	builder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("BuildStrict() = (%T, %v), want vars rejection", handler, err)
	}
	if !strings.Contains(err.Error(), routeID) || !strings.Contains(err.Error(), "vars") {
		t.Fatalf("BuildStrict() error = %q, want route ID and vars", err)
	}
}

func TestBuildStrictAllowsEmptyVarsAndRemoteAddrs(t *testing.T) {
	ensureRouteStore(t)
	effective := httpPluginAllowlist()
	for _, test := range []struct {
		routeID string
		payload string
	}{
		{routeID: "empty-vars", payload: `{"id":"empty-vars","uri":"/empty-vars","vars":[]}`},
		{routeID: "null-vars", payload: `{"id":"null-vars","uri":"/null-vars","vars":null}`},
		{routeID: "empty-remote", payload: `{"id":"empty-remote","uri":"/empty-remote","remote_addrs":[]}`},
	} {
		t.Run(test.routeID, func(t *testing.T) {
			putRouteResource(t, test.routeID, []byte(test.payload))
			builder := NewBuilder(nil, effective, testDataEncryptionResolver())
			t.Cleanup(builder.Stop)
			handler, err := builder.BuildStrict()
			if err != nil || handler == nil {
				t.Fatalf("BuildStrict() = (%T, %v), want empty %s accepted", handler, err, test.routeID)
			}
		})
	}
}

func TestBuildStrictRejectsVarsAndKeepsLastGoodHandler(t *testing.T) {
	ensureRouteStore(t)
	effective := httpPluginAllowlist()
	const routeID = "vars-last-good"
	putRouteResource(t, routeID, []byte(`{"id":"vars-last-good","uri":"/vars-last-good"}`))

	validBuilder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(validBuilder.Stop)
	lastGood, err := validBuilder.BuildStrict()
	if err != nil || lastGood == nil {
		t.Fatalf("valid BuildStrict() = (%T, %v)", lastGood, err)
	}

	putRouteResource(
		t,
		routeID,
		[]byte(`{"id":"vars-last-good","uri":"/vars-last-good","vars":[["http_user","==","ios"]]}`),
	)
	invalidBuilder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(invalidBuilder.Stop)
	handler, err := invalidBuilder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("invalid BuildStrict() = (%T, %v), want vars rejection", handler, err)
	}

	response := httptest.NewRecorder()
	lastGood.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/vars-last-good", nil))
	if response.Code == http.StatusNotFound {
		t.Fatalf("last-good handler status = %d, want the previously compiled route", response.Code)
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
			builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
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

func TestUnsupportedUpstreamSchemeAcceptsKafkaAcrossProfileAxes(t *testing.T) {
	effective := testEffectiveConfig()
	effective.Config.CompatibilityTarget = appconfig.CompatibilityAPISIX317
	effective.Config.SecurityProfile = appconfig.SecurityStrict
	effective.Config.QualificationProfile = appconfig.QualificationHTTPDataPlaneV1
	effective.Profiles = appconfig.ProfileSelection{
		Compatibility: appconfig.CompatibilityAPISIX317,
		Security:      appconfig.SecurityStrict,
		Qualification: appconfig.QualificationHTTPDataPlaneV1,
	}

	builder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	_, err := builder.buildReverseHandler(resource.Route{
		ID:       "profile-kafka-route",
		Upstream: resource.Upstream{Scheme: "kafka", Type: "chash"},
	}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v, want Kafka compatibility owner available across axes", err)
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
