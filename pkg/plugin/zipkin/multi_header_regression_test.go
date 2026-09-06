package zipkin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// x-b3-traceid / x-b3-spanid, then starts a new trace (HTTP 200). Only the
// single b3 header is a 400 (zipkin.t TEST 23 / zipkin2.t TEST 3).
func TestInvalidMultiHeaderB3Continues(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: "http://127.0.0.1:9411/api/v2/spans", SampleRatio: 1,
	})
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/echo", nil), time.Now(),
	)
	request.Header.Set("x-b3-traceid", "not-a-hex-trace-id")
	request.Header.Set("x-b3-spanid", "also-invalid")
	recorder := httptest.NewRecorder()
	result := p.RunRequestPhase(recorder, request)
	if recorder.Code == http.StatusBadRequest || result.Decision != base.RequestContinue {
		t.Fatalf(
			"status=%d decision=%d, want APISIX 3.17 continue after ignoring invalid multi-header B3",
			recorder.Code,
			result.Decision,
		)
	}
	_ = lifecycle
}
