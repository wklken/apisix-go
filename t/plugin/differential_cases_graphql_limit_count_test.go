package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialGraphQLLimitCountMatchesPinnedAPISIX317WrongMethod(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "graphql-limit-count-wrong-method-head",
		Plugin:  "graphql-limit-count",
		RouteID: "diff-graphql-limit-count-wrong-method",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-graphql-limit-count-wrong-method", "uri": "/hello",
				"plugins": map[string]any{"graphql-limit-count": map[string]any{
					"count": 4, "time_window": 60, "rejected_code": 503, "key": "remote_addr",
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodHead, Path: "/hello", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "upstream", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		ComparisonPolicy: differentialComparisonGraphQLHeadErrorContentType,
		SecurityDecision: "not_applicable",
	}}

	got := differentialGraphQLLimitCountCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialGraphQLLimitCountCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != differentialComparisonGraphQLHeadErrorContentType {
		t.Fatalf("comparison policy = %q", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
