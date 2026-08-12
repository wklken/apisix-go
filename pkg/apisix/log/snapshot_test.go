package log

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestBuildSnapshotDetachesMutableRequestAndResponseState(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://gateway.test/orders?q=one", strings.NewReader("request"))
	request = ctx.WithApisixVars(request, map[string]string{"$route_id": "route-1"})
	ctx.RegisterApisixVar(request, "$nested", map[string]any{"key": "value"})
	response := ResponseSnapshot{
		Header:  http.Header{"X-Trace": {"one"}},
		Trailer: http.Header{"X-Trailer": {"two"}},
		Body:    []byte("response"),
	}
	snapshot := BuildSnapshot(request, response, ctx.ResponseOutcome{Status: 201}, ctx.ResponseSourceUpstream,
		time.Unix(1, 0), time.Unix(2, 0))

	request.Header.Set("X-Trace", "mutated")
	response.Header.Set("X-Trace", "mutated")
	response.Body[0] = 'X'
	ctx.GetApisixVars(request)["$nested"].(map[string]any)["key"] = "mutated"
	if snapshot.Request.Header.Get("X-Trace") != "" {
		t.Fatalf("request snapshot unexpectedly copied response header: %#v", snapshot.Request.Header)
	}
	if snapshot.Response.Header.Get("X-Trace") != "one" || string(snapshot.Response.Body) != "response" {
		t.Fatalf("response snapshot changed after source mutation: %#v", snapshot.Response)
	}
	if got := snapshot.Request.APISIXVars["$nested"].(map[string]any)["key"]; got != "value" {
		t.Fatalf("nested APISIX var = %#v, want value", got)
	}
	if snapshot.Started != time.Unix(1, 0) || snapshot.Finished != time.Unix(2, 0) {
		t.Fatalf("snapshot timestamps = %v/%v", snapshot.Started, snapshot.Finished)
	}
}

func TestBuildSnapshotPreservesEffectiveRealIP(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil)
	request.RemoteAddr = "192.0.2.10:4321"
	request = request.WithContext(context.WithValue(request.Context(), ctx.RemoteAddrKey, "198.51.100.20"))
	request = request.WithContext(context.WithValue(request.Context(), ctx.RemotePortKey, "8443"))
	snapshot := BuildSnapshot(request, ResponseSnapshot{}, ctx.ResponseOutcome{}, ctx.ResponseSourceUpstream,
		time.Now(), time.Now())
	if snapshot.Request.APISIXVars["$remote_addr"] != "198.51.100.20" ||
		snapshot.Request.APISIXVars["$remote_port"] != "8443" {
		t.Fatalf("effective remote vars = %#v", snapshot.Request.APISIXVars)
	}
	fields := GetFieldsFromSnapshot(snapshot, map[string]string{
		"address": "$remote_addr", "port": "$remote_port",
	})
	if fields["address"] != "198.51.100.20" || fields["port"] != "8443" {
		t.Fatalf("detached remote fields = %#v", fields)
	}
}

func TestCloneSafeValueOmitsUnsafeValues(t *testing.T) {
	unsafePointer := new(int)
	source := map[string]any{
		"safe":    []any{"text", int64(2)},
		"func":    func() {},
		"pointer": unsafePointer,
		"struct":  struct{ Secret string }{Secret: "hidden"},
		"bytes":   []byte("secret"),
	}
	remaining := 1024
	cloned, ok := CloneSafeValue(source, &remaining)
	if !ok {
		t.Fatal("CloneSafeValue() rejected the safe map")
	}
	values := cloned.(map[string]any)
	if !reflect.DeepEqual(values["safe"], []any{"text", int64(2)}) {
		t.Fatalf("safe value = %#v", values["safe"])
	}
	for _, key := range []string{"func", "pointer", "struct", "bytes"} {
		if _, ok := values[key]; ok {
			t.Fatalf("unsafe key %q was retained: %#v", key, values)
		}
	}
}

func TestGetFieldsFromSnapshotUsesDetachedVariables(t *testing.T) {
	snapshot := LogSnapshot{
		Request: RequestLogSnapshot{
			Method:     http.MethodGet,
			URI:        "/orders?q=one",
			Host:       "gateway.test",
			Scheme:     "https",
			Header:     http.Header{"X-Request-Id": {"r-1"}},
			Query:      map[string][]string{"q": {"one"}},
			APISIXVars: map[string]any{"$route_id": "route-1"},
		},
		Outcome: ctx.ResponseOutcome{Status: http.StatusCreated, Bytes: 7},
		Source:  ctx.ResponseSourceEarlyStop,
	}
	fields := GetFieldsFromSnapshot(snapshot, map[string]string{
		"method": "$request_method", "route": "$route_id", "status": "$status",
		"query": "$arg_q", "request_id": "$http_x_request_id", "literal": "edge",
	})
	want := map[string]any{
		"method": http.MethodGet, "route": "route-1", "status": http.StatusCreated,
		"query": "one", "request_id": "r-1", "literal": "edge",
	}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
}

func TestGetFieldsFromSnapshotPreservesSupportedNginxVariableSemantics(t *testing.T) {
	finished := time.Date(2026, time.August, 12, 20, 15, 30, 0, time.FixedZone("CST", 8*60*60))
	snapshot := LogSnapshot{
		Request: RequestLogSnapshot{
			Method:        http.MethodPost,
			URI:           "/orders/42?q=one",
			Host:          "gateway.test:9443",
			RemoteAddr:    "192.0.2.10:4321",
			Scheme:        "https",
			Proto:         "HTTP/2.0",
			ContentLength: 7,
			Header: http.Header{
				"Content-Length":  {"7"},
				"Content-Type":    {"application/json"},
				"User-Agent":      {"snapshot-agent"},
				"Referer":         {"https://client.test/"},
				"X-Forwarded-For": {"198.51.100.1"},
			},
			Query: map[string][]string{"q": {"one"}},
		},
		Finished: finished,
	}
	fields := GetFieldsFromSnapshot(snapshot, map[string]string{
		"time_iso8601": "$time_iso8601", "time_local": "$time_local",
		"request_line": "$request_line", "request_uri": "$request_uri", "uri": "$uri",
		"host": "$host", "http_host": "$http_host", "remote_addr": "$remote_addr",
		"args": "$args", "query_string": "$query_string", "user_agent": "$http_user_agent",
		"referer": "$http_referer", "forwarded_for": "$http_x_forwarded_for",
		"content_length": "$content_length", "content_type": "$content_type",
	})
	want := map[string]any{
		"time_iso8601": finished.Format(time.RFC3339),
		"time_local":   finished.Format("02/Jan/2006:15:04:05 -0700"),
		"request_line": "POST /orders/42?q=one HTTP/2.0",
		"request_uri":  "/orders/42?q=one", "uri": "/orders/42",
		"host": "gateway.test", "http_host": "gateway.test:9443", "remote_addr": "192.0.2.10",
		"args": "q=one", "query_string": "q=one", "user_agent": "snapshot-agent",
		"referer": "https://client.test/", "forwarded_for": "198.51.100.1",
		"content_length": "7", "content_type": "application/json",
	}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
}
