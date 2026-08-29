package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialLDAPAuthCasesCoverPinnedAPISIX317MissingAuthorization(t *testing.T) {
	spec := assertDifferentialAuthCaseShape(
		t, differentialLDAPAuthCases(), "ldap-auth-missing-authorization", "ldap-auth", 0,
	)
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Headers["Authorization"] != "" {
		t.Fatalf("request = %#v, want GET /hello without Authorization", spec.Request)
	}
	route := spec.Config["routes"].([]any)[0].(map[string]any)
	auth := route["plugins"].(map[string]any)["ldap-auth"].(map[string]any)
	if auth["base_dn"] != "ou=users,dc=example,dc=org" || auth["uid"] != "cn" ||
		auth["ldap_uri"] != differentialFixturePlaceholder {
		t.Fatalf("ldap-auth config = %#v", auth)
	}
}
