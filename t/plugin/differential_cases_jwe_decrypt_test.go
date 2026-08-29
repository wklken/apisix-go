package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialJWEDecryptMatchesPinnedAPISIX317ForwardingVector(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	const token = "eyJraWQiOiJ1c2VyLWtleSIsImFsZyI6ImRpciIsImVuYyI6IkEyNTZHQ00ifQ..MTIzNDU2Nzg5MDEy.6JeRgm0.rNt131nG5wMvUD1KXbwLGA"
	want := []DifferentialCase{{
		Name:    "jwe-decrypt-forward-plaintext-header",
		Plugin:  "jwe-decrypt",
		RouteID: "diff-jwe-decrypt-forward-header",
		Config: map[string]any{
			"consumers": []any{map[string]any{
				"username": "jack",
				"plugins": map[string]any{"jwe-decrypt": map[string]any{
					"key": "user-key", "secret": "12345678901234567890123456789012",
				}},
			}},
			"routes": []any{map[string]any{
				"id": "diff-jwe-decrypt-forward-header", "uri": "/jwe",
				"plugins": map[string]any{"jwe-decrypt": map[string]any{
					"header": "Authorization", "forward_header": "Authorization",
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/jwe", Host: "gateway.example.test",
			Headers: map[string]string{"Authorization": "Bearer " + token},
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "decrypted"},
		},
		SecurityDecision: "allow",
	}}

	got := differentialJWEDecryptCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialJWEDecryptCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
