package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialAuthzKeycloakCasesCoverPinnedAPISIX317EmptyPermissionsDeny(t *testing.T) {
	spec := assertDifferentialAuthCaseShape(
		t, differentialAuthzKeycloakCases(), "authz-keycloak-enforcing-empty-permissions", "authz-keycloak", 0,
	)
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello1" ||
		spec.Request.Headers["Authorization"] != "Bearer fake access token" {
		t.Fatalf("request = %#v", spec.Request)
	}
	route := spec.Config["routes"].([]any)[0].(map[string]any)
	auth := route["plugins"].(map[string]any)["authz-keycloak"].(map[string]any)
	if auth["policy_enforcement_mode"] != "ENFORCING" || auth["client_id"] != "course_management" ||
		auth["token_endpoint"] != "http://"+differentialFixturePlaceholder+"/token" {
		t.Fatalf("authz-keycloak config = %#v", auth)
	}
}
