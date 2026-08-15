package base

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

func TestCollapseHeaderValuesLowercasesAndCopiesValues(t *testing.T) {
	header := http.Header{"X-Trace": {"first", "second"}}
	got := CollapseHeaderValues(header)
	values, ok := got["x-trace"].([]string)
	if !ok || len(values) != 2 || values[0] != "first" || values[1] != "second" {
		t.Fatalf("collapsed headers = %#v, want lowercase multi-value field", got)
	}
	values[0] = "changed"
	if header["X-Trace"][0] != "first" {
		t.Fatalf("CollapseHeaderValues() aliased source values: %#v", header)
	}
}

func TestBuildAccessLogFromSnapshotPreservesFullDefaultShape(t *testing.T) {
	started := time.Unix(100, 0)
	snapshot := LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			ID: "request-1", Method: http.MethodGet, URI: "/orders?q=one", URL: "/orders?q=one",
			Host: "gateway.test:9443", RemoteAddr: "192.0.2.3:3210", Scheme: "https",
			Header: http.Header{"X-Request-ID": {"r-1"}}, Query: map[string][]string{"q": {"one"}},
			APISIXVars: map[string]any{
				"$service_id": "service-1", "$route_id": "route-1",
				"$upstream_latency": int64(200), "$upstream_addr": "10.0.0.1:8080",
			},
			RequestVars: map[string]any{"$upstream_status": 201, "$retry_count": 2},
			Consumer:    apisixlog.SafeConsumerLogIdentity{Username: "alice"},
		},
		Response: apisixlog.ResponseLogSnapshot{Header: http.Header{"X-Result": {"ok"}}},
		Outcome: apisixctx.ResponseOutcome{
			Kind:   apisixctx.RequestOutcomeCompleted,
			Status: http.StatusCreated,
			Bytes:  9,
		},
		Source:   apisixctx.ResponseSourceUpstream,
		NodeID:   "node-1",
		Started:  started,
		Finished: started.Add(time.Second),
	}
	fields := BuildAccessLogFromSnapshot(snapshot, "")
	request := fields["request"].(map[string]any)
	response := fields["response"].(map[string]any)
	if request["url"] != "https://gateway.test:9443/orders?q=one" ||
		request["uri"] != "/orders?q=one" || response["status"] != http.StatusCreated {
		t.Fatalf("request/response fields = %#v / %#v", request, response)
	}
	if fields["upstream_latency"] != float64(200) || fields["apisix_latency"] != float64(800) ||
		fields["upstream"] != "10.0.0.1:8080" {
		t.Fatalf("latency/upstream fields = %#v", fields)
	}
	if fields["consumer"].(map[string]any)["username"] != "alice" {
		t.Fatalf("consumer = %#v", fields["consumer"])
	}
	if fields["request_id"] != "request-1" || fields["node_id"] != "node-1" ||
		fields["response_source"] != "upstream" || fields["outcome"] != "completed" ||
		fields["upstream_status"] != 201 || fields["retry_count"] != 2 {
		t.Fatalf("correlation fields = %#v", fields)
	}
}

func TestCaptureAccessLogRequest(t *testing.T) {
	started := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	r := httptest.NewRequest(http.MethodPost, "http://example.com/foo?x=1&x=2&y=3", nil)
	r.Host = "example.com:8080"
	r.RemoteAddr = "192.168.1.10:54321"
	r.ContentLength = 42
	r.Header.Set("X-Custom", "a")

	request := CaptureAccessLogRequest(r, started, "10.0.0.1:9080")

	if request.Method != http.MethodPost {
		t.Fatalf("Method = %q, want POST", request.Method)
	}
	if request.URI != "/foo?x=1&x=2&y=3" {
		t.Fatalf("URI = %q", request.URI)
	}
	if request.URL != "http://example.com:9080/foo?x=1&x=2&y=3" {
		t.Fatalf("URL = %q", request.URL)
	}
	if request.Host != "example.com" {
		t.Fatalf("Host = %q, want example.com", request.Host)
	}
	if request.ClientIP != "192.168.1.10" {
		t.Fatalf("ClientIP = %q", request.ClientIP)
	}
	if request.ContentLength != 42 {
		t.Fatalf("ContentLength = %d, want 42", request.ContentLength)
	}
	if got := request.Headers["x-custom"]; got != "a" {
		t.Fatalf("Headers[x-custom] = %v, want a", got)
	}
	if got := request.Headers["host"]; got != "example.com:8080" {
		t.Fatalf("Headers[host] = %v, want example.com:8080", got)
	}
	if got := request.QueryString["x"]; got == nil {
		t.Fatalf("QueryString[x] = nil, want multi-value")
	}
	if got := request.QueryString["y"]; got != "3" {
		t.Fatalf("QueryString[y] = %v, want 3", got)
	}
	if !request.Started.Equal(started) {
		t.Fatalf("Started = %v, want %v", request.Started, started)
	}
}

func TestBuildAccessLogSnapshot(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	r = apisixctx.WithApisixVars(r, map[string]string{
		"$service_id":    "svc-1",
		"$consumer_name": "user-1",
		"$balancer_ip":   "10.1.1.1",
		"$balancer_port": "8000",
	})
	started := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	request := AccessLogRequest{
		Method:        http.MethodGet,
		URI:           "/",
		URL:           "http://example.com/",
		Host:          "example.com",
		ClientIP:      "192.168.1.10",
		ContentLength: 0,
		Headers:       map[string]any{"host": "example.com"},
		QueryString:   map[string]any{},
		Started:       started,
	}
	responseHeaders := http.Header{"Content-Type": {"application/json"}}

	snapshot := BuildAccessLogSnapshot(request, http.StatusOK, responseHeaders, 15, "route-1", r, 2500*time.Microsecond)

	if got := snapshot["route_id"]; got != "route-1" {
		t.Fatalf("route_id = %v, want route-1", got)
	}
	if got := snapshot["service_id"]; got != "svc-1" {
		t.Fatalf("service_id = %v, want svc-1", got)
	}
	consumer, ok := snapshot["consumer"].(map[string]any)
	if !ok || consumer["username"] != "user-1" {
		t.Fatalf("consumer = %#v, want username user-1", snapshot["consumer"])
	}
	requestFields, ok := snapshot["request"].(map[string]any)
	if !ok || requestFields["url"] != "http://example.com/" || requestFields["method"] != http.MethodGet {
		t.Fatalf("request fields = %#v", snapshot["request"])
	}
	responseFields, ok := snapshot["response"].(map[string]any)
	if !ok || responseFields["status"] != http.StatusOK || responseFields["size"] != int64(15) {
		t.Fatalf("response fields = %#v", snapshot["response"])
	}
	responseHeadersCollapsed := responseFields["headers"].(map[string]any)
	if got := responseHeadersCollapsed["content-type"]; got != "application/json" {
		t.Fatalf("response headers = %#v", responseHeadersCollapsed)
	}
	if got := snapshot["upstream"]; got != "10.1.1.1:8000" {
		t.Fatalf("upstream = %v, want 10.1.1.1:8000", got)
	}
	if got := snapshot["latency"]; got != 2.5 {
		t.Fatalf("latency = %v, want 2.5ms", got)
	}
	if got := snapshot["apisix_latency"]; got != 2.5 {
		t.Fatalf("apisix_latency = %v, want 2.5", got)
	}
}
