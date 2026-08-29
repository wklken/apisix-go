package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAPIBreakerCasesCoverPinnedAPISIX317CustomResponse(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	request := DifferentialRequest{
		Method: http.MethodGet,
		Path:   "/api_breaker?code=500",
		Host:   "gateway.example.test",
	}
	want := []DifferentialCase{{
		Name:    "api-breaker-custom-response-after-threshold",
		Plugin:  "api-breaker",
		RouteID: "differential-api-breaker-custom-response",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  "differential-api-breaker-custom-response",
				"uri": "/api_breaker",
				"plugins": map[string]any{
					"api-breaker": map[string]any{
						"break_response_code": 502,
						"break_response_body": `{"message":"breaker opened."}`,
						"break_response_headers": []any{
							map[string]any{"key": "Content-Type", "value": "application/json"},
							map[string]any{"key": "Content-Type", "value": "application/json+v1"},
						},
						"unhealthy": map[string]any{"failures": 3},
						"healthy":   map[string]any{"successes": 3},
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{
			{Request: request, SecurityDecision: "not_applicable"},
			{Request: request, SecurityDecision: "not_applicable"},
			{Request: request, SecurityDecision: "not_applicable"},
			{Request: request, SecurityDecision: "not_applicable"},
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 3,
			Response: DifferentialFixtureResponse{
				Status: http.StatusInternalServerError,
				Body:   "upstream failure",
			},
		},
	}}

	got := differentialAPIBreakerCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialAPIBreakerCases() = %#v, want %#v", got, want)
	}
	if len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want <= 64", len(got[0].RouteID))
	}
	if got[0].Request.Method != "" || got[0].SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", got[0].Request, got[0].SecurityDecision)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
}
