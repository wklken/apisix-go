package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialServerInfoCasesCoverPinnedAPISIX317ControlAPI(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialServerInfoCases()
	if len(cases) != 1 {
		t.Fatalf("differentialServerInfoCases() = %d cases, want one", len(cases))
	}
	spec := cases[0]
	if spec.Name != "server-info-control-api-shape" || spec.Plugin != "server-info" ||
		spec.RouteID != "differential-server-info-control" ||
		spec.ComparisonPolicy != "server-info-control-api" {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.ComparisonPolicy)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/v1/server_info" ||
		spec.Request.Host != "gateway.example.test" ||
		spec.Request.Target != DifferentialRequestTargetControl {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.ExpectedCalls != 0 || spec.SecurityDecision != "not_applicable" {
		t.Fatalf("fixture/security = %d/%q", spec.Fixture.ExpectedCalls, spec.SecurityDecision)
	}
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 0 {
		t.Fatalf("routes = %#v, want an empty standalone route list", spec.Config["routes"])
	}
}
