package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialWolfRBACCasesCoverPinnedAPISIX317MissingToken(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "wolf-rbac-missing-token",
		Plugin:  "wolf-rbac",
		RouteID: "differential-wolf-rbac-missing-token",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":   "differential-wolf-rbac-missing-token",
				"uris": []any{"/hello*", "/wolf/rbac/*"},
				"plugins": map[string]any{
					"wolf-rbac": map[string]any{},
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
				Body:   "unexpected",
			},
		},
		SecurityDecision: "deny",
	}}

	got := differentialWolfRBACCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialWolfRBACCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
}
