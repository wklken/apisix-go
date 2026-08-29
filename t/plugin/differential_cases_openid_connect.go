package pluginintegration

import "net/http"

const differentialOIDCPublicKey = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEApKJGLzwwYx+YS4wnhXxq
VhuT4dcUe1h6MdLAl0n+7WKhN4WODkBR8Rz8Flcu70NI4xZOIv4ICUyBXmIIJdCw
5jgOIBjjZpEF1FOkQIBIF6tFhEo86sCgfjNWE53rJVSJ/453nDcy2jBiOf7vA82X
CVevdTT0glaHEYCftu82+6hT2kd7u0e/BZATU1VyJh5CpW4BksrhmGkMUv9NFCaa
vQiEaFj+Had/LooAb3LzDdi/n0Kad+ZDT9y2i76VAWc7j0nnC56zhDX8Nw5KesJr
YqivjlHaqmr772gwDVaGA2w74vzl27b4tKNG+GWeT2tXCFRorEmczgMjwoXcTscC
UwIDAQAB
-----END PUBLIC KEY-----`

// differentialOpenIDConnectCases maps the missing-token contract from APISIX
// 3.17 t/plugin/openid-connect.t TEST 11/12 onto the local-public-key mode set
// up by TEST 14. This avoids discovery I/O while still executing bearer_only.
func differentialOpenIDConnectCases() []DifferentialCase {
	const routeID = "differential-openid-connect-missing-bearer"
	return []DifferentialCase{{
		Name:             "openid-connect-bearer-only-missing-token",
		Plugin:           "openid-connect",
		RouteID:          routeID,
		ComparisonPolicy: differentialComparisonPlatformOwnedErrorRepresentation,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins": map[string]any{"openid-connect": map[string]any{
					"client_id":                         "integration-client",
					"discovery":                         "https://samples.auth0.com/.well-known/openid-configuration",
					"bearer_only":                       true,
					"public_key":                        differentialOIDCPublicKey,
					"token_signing_alg_values_expected": "RS256",
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/hello", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unexpected"},
		},
		SecurityDecision: "deny",
	}}
}
