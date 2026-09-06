package consumer

import "testing"

func TestParityJWTConsumerRequiresAlgorithmCredential(t *testing.T) {
	for _, algorithm := range []string{"", "HS256", "HS384", "HS512", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"} {
		config := map[string]any{"key": "test"}
		if algorithm != "" {
			config["algorithm"] = algorithm
		}
		if err := ValidateResolved("jwt-auth", config); err == nil {
			t.Errorf("%q accepted without credential", algorithm)
		}
		field := "public_key"
		if algorithm == "" || algorithm[:2] == "HS" {
			field = "secret"
		}
		config[field] = "synthetic"
		if err := ValidateResolved("jwt-auth", config); err != nil {
			t.Errorf("%q rejects required credential: %v", algorithm, err)
		}
	}
}
