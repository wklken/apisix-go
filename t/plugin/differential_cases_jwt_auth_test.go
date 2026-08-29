package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialJWTAuthMatchesPinnedAPISIX317NonExpiringHS256Vector(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	const token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJrZXkiOiJ1c2VyLWtleSJ9._7aoTZdzQDT0r9swHTcHb3nsujexcGjSTU-LRzTRVyY"
	want := []DifferentialCase{{
		Name:    "jwt-auth-hs256-token-without-exp",
		Plugin:  "jwt-auth",
		RouteID: "diff-jwt-auth-hs256-no-exp",
		Config: map[string]any{
			"consumers": []any{map[string]any{
				"username": "jack",
				"plugins": map[string]any{"jwt-auth": map[string]any{
					"key": "user-key", "secret": "my-secret-key", "algorithm": "HS256",
				}},
			}},
			"routes": []any{map[string]any{
				"id": "diff-jwt-auth-hs256-no-exp", "uri": "/jwt",
				"plugins":  map[string]any{"jwt-auth": map[string]any{}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/jwt?jwt=" + token, Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "authorized"},
		},
		SecurityDecision: "allow",
	}}

	got := differentialJWTAuthCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialJWTAuthCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
