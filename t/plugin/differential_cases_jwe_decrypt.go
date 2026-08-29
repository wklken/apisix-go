package pluginintegration

import "net/http"

// differentialJWEDecryptCases maps APISIX 3.17 t/plugin/jwe-decrypt.t
// TEST 6, TEST 8, and TEST 11 to the pinned decrypt-and-forward vector.
func differentialJWEDecryptCases() []DifferentialCase {
	const token = "eyJraWQiOiJ1c2VyLWtleSIsImFsZyI6ImRpciIsImVuYyI6IkEyNTZHQ00ifQ..MTIzNDU2Nzg5MDEy.6JeRgm0.rNt131nG5wMvUD1KXbwLGA"
	return []DifferentialCase{{
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
}
