package base

import (
	"net/http"
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

func TestCollapseAccessLogHeaderValuesRedactsSensitiveHeaders(t *testing.T) {
	got := CollapseAccessLogHeaderValues(testAccessLogHeaders())
	for _, name := range testSensitiveAccessLogHeaders {
		if _, ok := got[name]; ok {
			t.Fatalf("sensitive header %q = %#v, want omitted", name, got[name])
		}
	}
	if got["x-visible"] == nil {
		t.Fatalf("safe headers = %#v, want benign header", got)
	}
	if got["host"] != "gateway.test" {
		t.Fatalf("host = %#v, want preserved host header", got["host"])
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

func TestApplyMatchedRouteFieldsMatchesAPISIX317CustomLogFormat(t *testing.T) {
	fields := map[string]any{
		"case": "logger", "route_id": "configured-route", "service_id": "configured-service",
	}
	ApplyMatchedRouteFields(fields, "matched-route", "")

	if fields["route_id"] != "matched-route" {
		t.Fatalf("route_id = %#v, want matched-route", fields["route_id"])
	}
	if _, exists := fields["service_id"]; exists {
		t.Fatalf("service_id = %#v, want omitted when matched route has no service", fields["service_id"])
	}

	ApplyMatchedRouteFields(fields, "matched-route", "matched-service")
	if fields["service_id"] != "matched-service" {
		t.Fatalf("service_id = %#v, want matched-service", fields["service_id"])
	}

	unmatched := map[string]any{"route_id": "configured-route", "service_id": "configured-service"}
	ApplyMatchedRouteFields(unmatched, "", "")
	if unmatched["route_id"] != "configured-route" || unmatched["service_id"] != "configured-service" {
		t.Fatalf("unmatched fields = %#v, want configured values unchanged", unmatched)
	}
}

func TestBuildAccessLogFromSnapshotRedactsSensitiveHeaders(t *testing.T) {
	detached := BuildAccessLogFromSnapshot(LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodGet,
			URI:    "/orders",
			Host:   "gateway.test:9443",
			Header: testAccessLogHeaders(),
		},
		Response: apisixlog.ResponseLogSnapshot{Header: testAccessLogHeaders()},
		Started:  time.Unix(100, 0),
		Finished: time.Unix(101, 0),
	}, "route-1")
	assertSafeDefaultAccessLogHeaders(t, detached)
}

var testSensitiveAccessLogHeaders = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"set-cookie",
	"apikey",
	"x-api-key",
	"x-functions-key",
	"x-amz-security-token",
	"x-goog-api-key",
}

func testAccessLogHeaders() http.Header {
	return http.Header{
		"aUtHoRiZaTiOn":        {"secret-authorization"},
		"pRoXy-AuThOrIzAtIoN":  {"secret-proxy-authorization"},
		"cOoKiE":               {"secret-cookie"},
		"sEt-CoOkIe":           {"secret-set-cookie"},
		"aPiKeY":               {"secret-apikey"},
		"x-aPi-kEy":            {"secret-api-key"},
		"x-fUnCtIoNs-kEy":      {"secret-functions-key"},
		"x-aMz-SeCuRiTy-ToKeN": {"secret-amz-token"},
		"x-GoOg-aPi-KeY":       {"secret-goog-key"},
		"Host":                 {"gateway.test"},
		"X-Visible":            {"first", "second"},
	}
}

func assertSafeDefaultAccessLogHeaders(t *testing.T, fields map[string]any) {
	t.Helper()
	for _, section := range []string{"request", "response"} {
		payload, ok := fields[section].(map[string]any)
		if !ok {
			t.Fatalf("%s payload = %#v, want object", section, fields[section])
		}
		headers, ok := payload["headers"].(map[string]any)
		if !ok {
			t.Fatalf("%s headers = %#v, want object", section, payload["headers"])
		}
		for _, name := range testSensitiveAccessLogHeaders {
			if _, ok := headers[name]; ok {
				t.Fatalf("%s sensitive header %q = %#v, want omitted", section, name, headers[name])
			}
		}
		if got := headers["x-visible"]; got == nil {
			t.Fatalf("%s headers = %#v, want benign header", section, headers)
		}
	}
	request := fields["request"].(map[string]any)
	if request["headers"].(map[string]any)["host"] == nil {
		t.Fatalf("request headers = %#v, want host", request["headers"])
	}
}
