package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialACLMatchesPinnedAPISIX317BasicAuthLabelDeny(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "acl-basic-auth-nonmatching-label-denied",
		Plugin:  "acl",
		RouteID: "diff-acl-basic-auth-label-deny",
		Config: map[string]any{
			"consumers": []any{map[string]any{
				"username": "rose",
				"plugins": map[string]any{"basic-auth": map[string]any{
					"username": "rose", "password": "123456",
				}},
				"labels": map[string]any{
					"project": `["tomcat","web-server","http,server"]`,
				},
			}},
			"routes": []any{map[string]any{
				"id": "diff-acl-basic-auth-label-deny", "uri": "/acl",
				"plugins": map[string]any{
					"basic-auth": map[string]any{},
					"acl": map[string]any{
						"allow_labels":  map[string]any{"project": []any{"apisix"}},
						"rejected_code": 403,
						"rejected_msg":  "The consumer is forbidden.",
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/acl", Host: "gateway.example.test",
			Headers: map[string]string{"Authorization": "Basic cm9zZToxMjM0NTY="},
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		SecurityDecision: "deny",
	}}

	got := differentialACLCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialACLCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
	route := got[0].Config["routes"].([]any)[0].(map[string]any)
	acl := route["plugins"].(map[string]any)["acl"].(map[string]any)
	if acl["rejected_code"] != 403 || acl["rejected_msg"] != "The consumer is forbidden." {
		t.Fatalf("ACL rejection = %#v, want exact pinned 403/body", acl)
	}
}
