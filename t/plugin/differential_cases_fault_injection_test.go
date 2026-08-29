package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialFaultInjectionMatchesPinnedAPISIX317FixedAbort(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "fault-injection-fixed-abort",
		Plugin:  "fault-injection",
		RouteID: "diff-fault-injection-fixed-abort",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-fault-injection-fixed-abort", "uri": "/fault",
				"plugins": map[string]any{"fault-injection": map[string]any{
					"abort": map[string]any{
						"http_status": 405,
						"body":        "Fault Injection!\n",
					},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/fault", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		SecurityDecision: "not_applicable",
	}}

	got := differentialFaultInjectionCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialFaultInjectionCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
