package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialPublicAPICasesDispatchPinnedWolfRBACUserInfo(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialPublicAPICases()
	if len(cases) != 1 {
		t.Fatalf("differentialPublicAPICases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "public-api-wolf-rbac-userinfo-missing-token" || spec.Plugin != "public-api" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-public-api-userinfo" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.Request.Method != http.MethodGet ||
		spec.Request.Path != "/apisix/plugin/wolf-rbac/user_info" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 0 ||
		spec.Fixture.Response.Status != http.StatusOK {
		t.Fatalf("fixture = %#v, want unused 200 upstream", spec.Fixture)
	}
	if spec.SecurityDecision != "deny" || spec.ComparisonPolicy != "" {
		t.Fatalf(
			"decision/policy = %q/%q, want deny/exact",
			spec.SecurityDecision,
			spec.ComparisonPolicy,
		)
	}
	if got, want := differentialRequiredPluginNames(
		cases,
	), []string{
		"public-api",
		"wolf-rbac",
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("required plugins = %v, want %v", got, want)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 2 {
		t.Fatalf("routes = %#v, want wolf initializer and public-api route", spec.Config["routes"])
	}
	initializer := routes[0].(map[string]any)
	if initializer["id"] != "differential-public-api-wolf-init" || initializer["uri"] != "/_wolf-init" {
		t.Fatalf("wolf initializer = %#v", initializer)
	}
	if wolf, ok := initializer["plugins"].(map[string]any)["wolf-rbac"].(map[string]any); !ok || len(wolf) != 0 {
		t.Fatalf("wolf initializer plugins = %#v", initializer["plugins"])
	}

	dispatch := routes[1].(map[string]any)
	if dispatch["id"] != spec.RouteID || dispatch["uri"] != spec.Request.Path {
		t.Fatalf("public-api route = %#v", dispatch)
	}
	if publicAPI, ok := dispatch["plugins"].(map[string]any)["public-api"].(map[string]any); !ok ||
		len(publicAPI) != 0 {
		t.Fatalf("public-api config = %#v", dispatch["plugins"])
	}
	if !reflect.DeepEqual(dispatch["upstream"], differentialUpstream()) {
		t.Fatalf("public-api fallback upstream = %#v, want deterministic fixture", dispatch["upstream"])
	}
}
