package expr

import (
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestAPISIXRequestIDDefaultsToNativeRequestID(t *testing.T) {
	request, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest("GET", "/", nil), time.Now())
	native, _ := RequestValue(request, "request_id").(string)
	apisixID, _ := RequestValue(request, "apisix_request_id").(string)
	t.Logf("request_id=%q apisix_request_id=%q", native, apisixID)
	if native == "" || apisixID != native {
		t.Fatalf("apisix_request_id=%q, want native request_id=%q before request-id plugin", apisixID, native)
	}
}
