package log

import (
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestAPISIXRequestIDDefaultResolvesThroughLiveLogField(t *testing.T) {
	request, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest("GET", "/", nil), time.Now())
	native, _ := apisixctx.GetRequestVar(request, "$request_id").(string)
	if got := GetField(request, "$apisix_request_id"); got != native {
		t.Fatalf(
			"GetField($apisix_request_id) = %#v, want native request ID %q; APISIX map value=%#v request map value=%#v",
			got,
			native,
			apisixctx.GetApisixVar(request, "$apisix_request_id"),
			apisixctx.GetRequestVar(request, "$apisix_request_id"),
		)
	}
}

func TestDefaultNativeRequestIDPopulatesCorrelation(t *testing.T) {
	request, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest("GET", "/", nil), time.Now())
	native, _ := apisixctx.GetRequestVar(request, "$request_id").(string)
	correlation := CaptureRequestCorrelation(request)
	if correlation.RequestID != native {
		t.Fatalf("CaptureRequestCorrelation().RequestID = %q, want native request ID %q", correlation.RequestID, native)
	}
}

func TestAPISIXRequestIDFallbackPopulatesCorrelation(t *testing.T) {
	request := apisixctx.WithApisixVars(httptest.NewRequest("GET", "/", nil), map[string]string{
		"$apisix_request_id": "selected-plugin-id",
		"$request_id":        "stale-native-id",
	})
	if got := CaptureRequestCorrelation(request).RequestID; got != "selected-plugin-id" {
		t.Fatalf("correlation ID = %q, want selected-plugin-id", got)
	}
}
