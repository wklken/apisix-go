package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialLimitConnCasesCoverPinnedGlobalSharedCapacity(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	cases := differentialLimitConnCases()
	if len(cases) != 1 {
		t.Fatalf("limit-conn differential cases = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "limit-conn-global-rule-shares-capacity-across-routes" ||
		spec.Plugin != "limit-conn" ||
		spec.RouteID != "differential-limit-conn-route-a" ||
		spec.ComparisonPolicy != differentialLimitConnGlobalSharedCapacityPolicy {
		t.Fatalf("limit-conn case identity = %#v", spec)
	}

	rules, ok := spec.Config["global_rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("global rules = %#v, want one", spec.Config["global_rules"])
	}
	rule, ok := rules[0].(map[string]any)
	if !ok || rule["id"] != "differential-limit-conn-global" {
		t.Fatalf("global rule = %#v", rules[0])
	}
	plugins, ok := rule["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("global rule plugins = %#v", rule["plugins"])
	}
	config, ok := plugins["limit-conn"].(map[string]any)
	if !ok || config["conn"] != 2 || config["burst"] != 1 ||
		config["default_conn_delay"] != 0.1 || config["key"] != "remote_addr" ||
		config["rejected_code"] != http.StatusServiceUnavailable {
		t.Fatalf("limit-conn global config = %#v", config)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 2 {
		t.Fatalf("routes = %#v, want two", spec.Config["routes"])
	}
	for index, want := range []struct {
		id   string
		path string
	}{
		{id: "differential-limit-conn-route-a", path: "/limit-a"},
		{id: "differential-limit-conn-route-b", path: "/limit-b"},
	} {
		route, ok := routes[index].(map[string]any)
		if !ok || route["id"] != want.id || route["uri"] != want.path {
			t.Fatalf("route %d = %#v", index, routes[index])
		}
		if _, exists := route["plugins"]; exists {
			t.Fatalf("route %d unexpectedly owns plugins: %#v", index, route["plugins"])
		}
	}

	if len(spec.Steps) != 2 || len(spec.Steps[0].ConcurrentRequests) != 10 {
		t.Fatalf("limit-conn steps = %#v", spec.Steps)
	}
	paths := map[string]int{}
	for _, request := range spec.Steps[0].ConcurrentRequests {
		if request.Method != http.MethodGet || request.Host != "gateway.example.test" {
			t.Fatalf("concurrent request = %#v", request)
		}
		paths[request.Path]++
	}
	if paths["/limit-a"] != 5 || paths["/limit-b"] != 5 || len(paths) != 2 ||
		spec.Steps[0].SecurityDecision != "mixed" {
		t.Fatalf("concurrent request paths/decision = %#v/%q", paths, spec.Steps[0].SecurityDecision)
	}
	probe := spec.Steps[1]
	if len(probe.ConcurrentRequests) != 0 || probe.Request.Method != http.MethodGet ||
		probe.Request.Path != "/limit-b" || probe.Request.Host != "gateway.example.test" ||
		probe.SecurityDecision != "allow" {
		t.Fatalf("release probe = %#v", probe)
	}
	if spec.Fixture.ExpectedCalls != 4 || spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "text/plain" ||
		spec.Fixture.Response.Body != "hello world" || spec.Fixture.Response.DelayMillis != 1500 {
		t.Fatalf("limit-conn fixture = %#v", spec.Fixture)
	}
}
