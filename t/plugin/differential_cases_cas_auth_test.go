package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialCASAuthCasesCoverPinnedAPISIX317CallbackWithoutInitiationCookie(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	const routeID = "differential-cas-auth-callback-no-cookie"
	want := []DifferentialCase{{
		Name:             "cas-auth-callback-without-initiation-cookie",
		Plugin:           "cas-auth",
		RouteID:          routeID,
		ComparisonPolicy: "cas-auth-callback-nosniff",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "methods": []any{http.MethodGet, http.MethodPost},
				"host": "127.0.0.3", "uri": "/*",
				"plugins": map[string]any{"cas-auth": map[string]any{
					"idp_uri":          "http://" + differentialFixturePlaceholder + "/realms/test/protocol/cas",
					"cas_callback_uri": "/cas_callback",
					"logout_uri":       "/logout",
					"cookie": map[string]any{
						"secret": "0123456789abcdef0123456789abcdef",
						"secure": false,
					},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/cas_callback?ticket=ST-test",
			Host:   "127.0.0.3",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected network call",
			},
		},
		SecurityDecision: "deny",
	}}

	if got := differentialCASAuthCases(); !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialCASAuthCases() = %#v, want %#v", got, want)
	}
}
