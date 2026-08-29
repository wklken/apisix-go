package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialBodyTransformerMatchesPinnedAPISIX317JSONRequestRewrite(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "body-transformer-json-request-rewrite",
		Plugin:  "body-transformer",
		RouteID: "diff-body-transformer-json-request",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-body-transformer-json-request", "uri": "/transform",
				"plugins": map[string]any{"body-transformer": map[string]any{
					"request": map[string]any{
						"input_format": "json",
						"template":     `{"bar":{{age+10}}}`,
					},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/transform", Host: "gateway.example.test",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"age":20}`,
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "transformed"},
		},
		SecurityDecision: "not_applicable",
	}}

	got := differentialBodyTransformerCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialBodyTransformerCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
