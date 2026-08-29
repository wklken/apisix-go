package route

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestCompileHTTPRejectsUnsupportedRouteSemantics(t *testing.T) {
	for _, test := range []struct {
		name    string
		field   string
		value   string
		routeID string
	}{
		{name: "script", field: "script", value: `"return true"`, routeID: "unsupported-script-route"},
		{name: "filter_func", field: "filter_func", value: `"return true"`, routeID: "unsupported-filter-route"},
		{name: "vars", field: "vars", value: `[["http_user","==","ios"]]`, routeID: "unsupported-vars-route"},
		{name: "remote_addrs", field: "remote_addrs", value: `["10.0.0.1"]`, routeID: "unsupported-remote-addrs-route"},
		{name: "remote_addr", field: "remote_addr", value: `"10.0.0.1"`, routeID: "unsupported-remote-addr-route"},
		{name: "script_id", field: "script_id", value: `"script-1"`, routeID: "unsupported-script-id-route"},
	} {
		t.Run(test.name, func(t *testing.T) {
			routeResource := testRouteFromJSON(t,
				`{"id":"`+test.routeID+`","uri":"/`+test.routeID+`","`+test.field+`":`+test.value+`}`,
			)
			snapshot, err := CompileHTTP(context.Background(), CompileInput{
				Revision: 1,
				Routes:   testPreparedRoutes(routeResource),
			})
			if err == nil || snapshot != nil {
				t.Fatalf("CompileHTTP() = (%T, %v), want unsupported %s rejection", snapshot, err, test.field)
			}
			if !strings.Contains(err.Error(), test.routeID) || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("CompileHTTP() error = %q, want route ID %q and field %q", err, test.routeID, test.field)
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

func TestCompileHTTPAllowsBlankFilterFunc(t *testing.T) {
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: testPreparedRoutes(testRouteFromJSON(t,
			`{"id":"blank-filter-route","uri":"/blank-filter-route","filter_func":" \t "}`,
		)),
	})
	if err != nil || snapshot == nil {
		t.Fatalf("CompileHTTP() = (%T, %v), want blank filter_func accepted", snapshot, err)
	}
}

func TestCompileHTTPRejectsNestedNumericVars(t *testing.T) {
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: testPreparedRoutes(testRouteFromJSON(t,
			`{"id":"nested-numeric-vars","uri":"/nested-numeric-vars","vars":[["arg_age","==",18]]}`,
		)),
	})
	if err == nil || snapshot != nil {
		t.Fatalf("CompileHTTP() = (%T, %v), want vars rejection", snapshot, err)
	}
	if !strings.Contains(err.Error(), "nested-numeric-vars") || !strings.Contains(err.Error(), "vars") {
		t.Fatalf("CompileHTTP() error = %q, want route ID and vars", err)
	}
}

func TestCompileHTTPAllowsEmptyVarsAndRemoteAddrs(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "empty-vars", raw: `{"id":"empty-vars","uri":"/empty-vars","vars":[]}`},
		{name: "null-vars", raw: `{"id":"null-vars","uri":"/null-vars","vars":null}`},
		{name: "empty-remote", raw: `{"id":"empty-remote","uri":"/empty-remote","remote_addrs":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := CompileHTTP(context.Background(), CompileInput{
				Revision: 1,
				Routes:   testPreparedRoutes(testRouteFromJSON(t, test.raw)),
			})
			if err != nil || snapshot == nil {
				t.Fatalf("CompileHTTP() = (%T, %v), want empty field accepted", snapshot, err)
			}
		})
	}
}

func TestPlanRouteUpstreamValidatesHTTPUpstreamTypes(t *testing.T) {
	for _, test := range []struct {
		name   string
		scheme string
		type_  string
		wantOK bool
	}{
		{name: "default scheme empty type", wantOK: true},
		{name: "http roundrobin", scheme: "http", type_: "roundrobin", wantOK: true},
		{name: "https empty type", scheme: "https", wantOK: true},
		{name: "grpc roundrobin", scheme: "grpc", type_: "roundrobin", wantOK: true},
		{name: "grpcs empty type", scheme: "grpcs", wantOK: true},
		{name: "http chash", scheme: "http", type_: "chash"},
		{name: "http random", scheme: "http", type_: "random"},
		{name: "kafka owner", scheme: "kafka", type_: "chash", wantOK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := PlanRouteUpstream(
				resource.Route{ID: "upstream-type", Upstream: resource.Upstream{Scheme: test.scheme, Type: test.type_}},
				resource.Service{}, nil, nil, &testEffectiveConfig().Config,
			)
			if test.wantOK {
				if err != nil {
					t.Fatalf("PlanRouteUpstream() error = %v, want accepted upstream type", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "unsupported upstream type") {
				t.Fatalf("PlanRouteUpstream() error = %v, want unsupported upstream type", err)
			}
		})
	}
}

func TestPlanRouteUpstreamAcceptsKafka(t *testing.T) {
	static := testEffectiveConfig().Config
	_, err := PlanRouteUpstream(
		resource.Route{ID: "kafka-route", Upstream: resource.Upstream{Scheme: "kafka", Type: "chash"}},
		resource.Service{}, nil, nil, &static,
	)
	if err != nil {
		t.Fatalf("PlanRouteUpstream() error = %v, want Kafka owner available", err)
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
