package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialURIBlockerCaseMatchesPinnedAPISIX317RejectedMessage(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{
		{
			Name:    "uri-blocker-rejects-matching-query",
			Plugin:  "uri-blocker",
			RouteID: "diff-uri-blocker-query-reject",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-uri-blocker-query-reject",
					"uri": "/hello",
					"plugins": map[string]any{
						"uri-blocker": map[string]any{
							"block_rules":  []any{"aa"},
							"rejected_msg": "access is not allowed",
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello?aa=1",
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
			SecurityDecision: "deny",
		},
	}

	got := differentialURIBlockerCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialURIBlockerCases() = %#v, want %#v", got, want)
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
