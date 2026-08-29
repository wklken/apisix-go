package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialAuthzCasdoorCasesCoverPinnedAPISIX317CallbackWithoutSession(t *testing.T) {
	spec := assertDifferentialAuthCaseShapeWithPolicy(
		t, differentialAuthzCasdoorCases(), "authz-casdoor-callback-without-session", "authz-casdoor", 0,
		differentialComparisonPlatformOwnedErrorRepresentation,
	)
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/anything/callback?code=aaa&state=bbb" {
		t.Fatalf("request = %#v", spec.Request)
	}
	route := spec.Config["routes"].([]any)[0].(map[string]any)
	auth := route["plugins"].(map[string]any)["authz-casdoor"].(map[string]any)
	if auth["callback_url"] != "http://gateway.example.test/anything/callback" ||
		auth["endpoint_addr"] != "http://"+differentialFixturePlaceholder {
		t.Fatalf("authz-casdoor config = %#v", auth)
	}
}
