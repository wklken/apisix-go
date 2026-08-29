package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialRequestIDCasesMatchPinnedAPISIX317ResponseHeaderBlocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialRequestIDCases()
	if len(cases) != 2 {
		t.Fatalf("differentialRequestIDCases() = %d cases, want 2", len(cases))
	}

	want := []DifferentialCase{
		{
			Name:    "request-id-preserves-client-id-in-response",
			Plugin:  "request-id",
			RouteID: "differential-request-id-preserve-client",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "differential-request-id-preserve-client",
					"uri": "/opentracing",
					"plugins": map[string]any{
						"request-id": map[string]any{"include_in_response": true},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/opentracing",
				Host:    "gateway.example.test",
				Headers: map[string]string{"X-Request-Id": "client-provided-id"},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "ok",
				},
			},
			SecurityDecision: "not_applicable",
		},
		{
			Name:    "request-id-omits-client-id-from-response",
			Plugin:  "request-id",
			RouteID: "differential-request-id-omit-response",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "differential-request-id-omit-response",
					"uri": "/opentracing",
					"plugins": map[string]any{
						"request-id": map[string]any{"include_in_response": false},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/opentracing",
				Host:    "gateway.example.test",
				Headers: map[string]string{"X-Request-Id": "client-provided-id"},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "ok",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}

	if !reflect.DeepEqual(cases, want) {
		t.Fatalf("differentialRequestIDCases() = %#v, want %#v", cases, want)
	}
	for _, spec := range cases {
		if spec.ComparisonPolicy != "" {
			t.Fatalf("case %q comparison policy = %q, want exact", spec.Name, spec.ComparisonPolicy)
		}
		if len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want <= 64", spec.Name, len(spec.RouteID))
		}
	}
}
