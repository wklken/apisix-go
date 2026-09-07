package skywalking

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestExitSpanOmitsMultiAttemptUpstreamStatus(t *testing.T) {
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil))
	apisixctx.RegisterRequestVar(req, "$upstream_status", "502, 200")
	plugin := &Plugin{config: Config{ServiceName: "APISIX", ServiceInstanceName: "APISIX Instance Name"}}
	segment := plugin.buildSegment(
		sw8Context{TraceID: "trace", TraceSegmentID: "segment"},
		req,
		http.StatusOK,
		time.Unix(100, 0),
		time.Second,
	)
	if len(segment.Spans) != 2 {
		t.Fatalf("spans = %d, want Entry and Exit", len(segment.Spans))
	}
	for _, tag := range segment.Spans[1].Tags {
		if tag.Key == "http.status_code" {
			t.Fatalf(
				"Exit http.status_code = %q; APISIX 3.17 tonumber(\"502, 200\") is nil and omits the tag",
				tag.Value,
			)
		}
	}
}
