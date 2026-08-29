package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialGraphQLProxyCacheMatchesPinnedAPISIX317WrongMethod(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "graphql-proxy-cache-wrong-method-head",
		Plugin:  "graphql-proxy-cache",
		RouteID: "diff-graphql-proxy-cache-wrong-method",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-graphql-proxy-cache-wrong-method", "uri": "/graphql",
				"plugins":  map[string]any{"graphql-proxy-cache": map[string]any{}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodHead, Path: "/graphql", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "origin", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		ComparisonPolicy: differentialComparisonGraphQLHeadErrorContentType,
		SecurityDecision: "not_applicable",
	}}

	got := differentialGraphQLProxyCacheCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialGraphQLProxyCacheCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != differentialComparisonGraphQLHeadErrorContentType {
		t.Fatalf("comparison policy = %q", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
