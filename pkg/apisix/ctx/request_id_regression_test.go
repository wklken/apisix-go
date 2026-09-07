package ctx

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeRequestIDIsStableAndRequestScoped(t *testing.T) {
	seen := map[string]bool{}
	for index := range 4 {
		request := httptest.NewRequest("GET", "/", nil)
		if index%2 == 0 {
			request = WithRequestVars(request)
		} else {
			request, _ = EnsureRequestLifecycle(request, time.Now())
		}
		id, _ := GetRequestVar(request, "$request_id").(string)
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" || seen[id] {
			t.Fatalf("invalid or repeated native request ID %q", id)
		}
		seen[id] = true
		if got := GetRequestVar(WithRequestVars(request), "$request_id"); got != id {
			t.Fatalf("request ID changed: %v -> %v", id, got)
		}
		RecycleVars(request)
	}
}

func TestRequestVariableInitializationPreservesPluginID(t *testing.T) {
	request := WithRequestVars(httptest.NewRequest("GET", "/", nil))
	native := GetRequestVar(request, "$request_id")
	request = WithApisixVars(request, map[string]string{"$apisix_request_id": "plugin-id"})
	request = WithRequestVars(request)
	if GetApisixVar(request, "$apisix_request_id") != "plugin-id" ||
		GetRequestVar(request, "$apisix_request_id") != "plugin-id" ||
		GetRequestVar(request, "$request_id") != native {
		t.Fatalf("request variables lost plugin/native identity: %+v", GetRequestVars(request))
	}
	RecycleVars(request)
}
