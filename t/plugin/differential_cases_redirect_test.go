package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialRedirectCaseMatchesPinnedAPISIX317FixedURIRedirect(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{
		{
			Name:             "redirect-fixed-uri-301",
			Plugin:           "redirect",
			RouteID:          "diff-redirect-fixed-uri",
			ComparisonPolicy: differentialComparisonPlatformOwnedRedirectRepresentation,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-redirect-fixed-uri",
					"uri": "/hello",
					"plugins": map[string]any{
						"redirect": map[string]any{
							"uri":      "/test/add",
							"ret_code": 301,
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

	got := differentialRedirectCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialRedirectCases() = %#v, want %#v", got, want)
	}
	for _, spec := range got {
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if spec.ComparisonPolicy != differentialComparisonPlatformOwnedRedirectRepresentation {
			t.Fatalf(
				"case %q comparison policy = %q, want redirect platform boundary",
				spec.Name,
				spec.ComparisonPolicy,
			)
		}
	}
}
