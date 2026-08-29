package pluginintegration

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialOpenIDConnectCasesCoverPinnedAPISIX317MissingBearerToken(t *testing.T) {
	spec := assertDifferentialAuthCaseShapeWithPolicy(
		t, differentialOpenIDConnectCases(), "openid-connect-bearer-only-missing-token", "openid-connect", 0,
		differentialComparisonPlatformOwnedErrorRepresentation,
	)
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Headers["Authorization"] != "" {
		t.Fatalf("request = %#v, want GET /hello without Authorization", spec.Request)
	}
	route := spec.Config["routes"].([]any)[0].(map[string]any)
	auth := route["plugins"].(map[string]any)["openid-connect"].(map[string]any)
	if auth["bearer_only"] != true || auth["client_id"] != "integration-client" ||
		auth["discovery"] != "https://samples.auth0.com/.well-known/openid-configuration" ||
		!strings.Contains(auth["public_key"].(string), "BEGIN PUBLIC KEY") {
		t.Fatalf("openid-connect config = %#v", auth)
	}
	block, _ := pem.Decode([]byte(auth["public_key"].(string)))
	if block == nil {
		t.Fatal("openid-connect public key is not PEM")
	}
	if _, err := x509.ParsePKIXPublicKey(block.Bytes); err != nil {
		t.Fatalf("parse openid-connect public key: %v", err)
	}
}
