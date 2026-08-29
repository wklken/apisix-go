package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialMockingCaseMatchesPinnedAPISIX317FixedExample(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{
		{
			Name:    "mocking-returns-fixed-example",
			Plugin:  "mocking",
			RouteID: "diff-mocking-fixed-example",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-mocking-fixed-example",
					"uri": "/hello",
					"plugins": map[string]any{
						"mocking": map[string]any{
							"content_type":     "text/plain",
							"response_status":  200,
							"response_example": "hello world",
							"with_mock_header": false,
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello",
				Host:   "gateway.example.test",
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 0,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "unused",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}

	got := differentialMockingCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialMockingCases() = %#v, want %#v", got, want)
	}
	for _, spec := range got {
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if spec.ComparisonPolicy != "" {
			t.Fatalf("case %q comparison policy = %q, want exact", spec.Name, spec.ComparisonPolicy)
		}
	}
}
