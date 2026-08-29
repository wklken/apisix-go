package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialAuthzCasbinCasesCoverPinnedAPISIX317MissingUsername(t *testing.T) {
	spec := assertDifferentialAuthCaseShape(
		t, differentialAuthzCasbinCases(), "authz-casbin-missing-username", "authz-casbin", 0,
	)
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" || spec.Request.Headers["user"] != "" {
		t.Fatalf("request = %#v, want GET /hello without user", spec.Request)
	}
	metadata := spec.Config["plugin_metadata"].([]any)[0].(map[string]any)
	if metadata["id"] != "authz-casbin" || !strings.Contains(metadata["model"].(string), "r = sub, obj, act") ||
		!strings.Contains(metadata["policy"].(string), "p, admin, *, *") {
		t.Fatalf("authz-casbin metadata = %#v", metadata)
	}
}
