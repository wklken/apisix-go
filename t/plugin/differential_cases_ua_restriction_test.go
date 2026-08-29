package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialUARestrictionCaseMatchesPinnedAPISIX317Denylist(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{
		{
			Name:    "ua-restriction-denies-listed-agent",
			Plugin:  "ua-restriction",
			RouteID: "diff-ua-restriction-deny",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-ua-restriction-deny",
					"uri": "/hello",
					"plugins": map[string]any{
						"ua-restriction": map[string]any{
							"denylist": []any{
								"my-bot1",
								`(Baiduspider)/(\d+)\.(\d+)`,
							},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/hello",
				Host:    "gateway.example.test",
				Headers: map[string]string{"User-Agent": "my-bot1"},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 0,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "unused",
				},
			},
			SecurityDecision: "deny",
		},
	}

	got := differentialUARestrictionCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialUARestrictionCases() = %#v, want %#v", got, want)
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
