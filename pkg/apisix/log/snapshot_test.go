package log

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestBuildSnapshotDetachesMutableRequestAndResponseState(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://gateway.test/orders?q=one", strings.NewReader("request"))
	request = request.WithContext(context.WithValue(request.Context(), ctx.RequestIDKey, "request-1"))
	request = ctx.WithApisixVars(request, map[string]string{"$route_id": "route-1", "$node_id": "node-1"})
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
	if snapshot.Request.ID != "request-1" || snapshot.NodeID != "node-1" {
		t.Fatalf("snapshot correlation = request %q node %q", snapshot.Request.ID, snapshot.NodeID)
	}
}

func TestBuildSnapshotFromOwnedInputsUsesCapturedBodyWithoutReadingRequest(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://gateway.test/orders?q=one", nil)
	request.Body = snapshotPanicBody{}
	response := ResponseSnapshot{
		Header:  http.Header{"X-Trace": {"one"}},
		Trailer: http.Header{"X-Trailer": {"two"}},
		Body:    []byte("response"),
	}
	capturedBody := []byte("captured-request")
	snapshot := BuildSnapshotFromOwnedInputs(
		request,
		response,
		capturedBody,
		true,
		ctx.ResponseOutcome{Status: http.StatusCreated},
		ctx.ResponseSourceUpstream,
		time.Unix(1, 0),
		time.Unix(2, 0),
	)
	if got := string(snapshot.Request.Body); got != "captured-request" || !snapshot.Request.BodyTruncated {
		t.Fatalf("captured request body = %q/truncated=%t", got, snapshot.Request.BodyTruncated)
	}
	if got := string(snapshot.Response.Body); got != "response" ||
		snapshot.Response.Header.Get("X-Trace") != "one" ||
		snapshot.Response.Trailer.Get("X-Trailer") != "two" {
		t.Fatalf("owned response capture = %#v", snapshot.Response)
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

func TestCloneSafeValueHonorsKindsDepthAndBudget(t *testing.T) {
	remaining := 128
	value, ok := CloneSafeValue(map[string]any{
		"bool":  true,
		"int":   int32(7),
		"uint":  uint16(8),
		"float": float32(1.5),
		"array": [2]string{"a", "b"},
		"nil":   nil,
	}, &remaining)
	if !ok {
		t.Fatal("CloneSafeValue() rejected supported JSON-like values")
	}
	cloned := value.(map[string]any)
	if cloned["bool"] != true || cloned["int"] != int32(7) || cloned["uint"] != uint16(8) ||
		cloned["float"] != float32(1.5) || !reflect.DeepEqual(cloned["array"], []any{"a", "b"}) {
		t.Fatalf("cloned values = %#v", cloned)
	}
	if cloned["nil"] != nil {
		t.Fatalf("cloned nil = %#v", cloned["nil"])
	}

	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "nil map", value: map[string]any(nil)},
		{name: "non-string map", value: map[int]string{1: "one"}},
		{name: "bytes", value: []byte("binary")},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := 32
			if _, ok := CloneSafeValue(test.value, &budget); ok {
				t.Fatalf("CloneSafeValue(%T) unexpectedly succeeded", test.value)
			}
		})
	}

	zero := 0
	for _, value := range []any{true, "x", int64(1), uint64(1), float64(1), []any{"x"}, map[string]any{"x": 1}} {
		if _, ok := CloneSafeValue(value, &zero); ok {
			t.Fatalf("CloneSafeValue(%T) ignored exhausted budget", value)
		}
	}
	if consumeSnapshotBudget(nil, 1) != true || consumeSnapshotBudget(&zero, -1) != false {
		t.Fatal("consumeSnapshotBudget() boundary semantics changed")
	}
	if _, ok := cloneSafeValue(reflect.ValueOf("deep"), nil, 65); ok {
		t.Fatal("cloneSafeValue() accepted a value beyond the depth limit")
	}
}

func TestCloneSnapshotIsolatesEveryMutableField(t *testing.T) {
	original := LogSnapshot{
		Request: RequestLogSnapshot{
			Header:      http.Header{"X-Request": {"one"}},
			Query:       url.Values{"q": {"one"}},
			Body:        []byte("request"),
			APISIXVars:  map[string]any{"$nested": map[string]any{"key": "one"}},
			RequestVars: map[string]any{"$list": []any{"one"}},
		},
		Response: ResponseLogSnapshot{
			Header:  http.Header{"X-Response": {"one"}},
			Trailer: http.Header{"X-Trailer": {"one"}},
			Body:    []byte("response"),
		},
	}
	clone := CloneSnapshot(original)
	clone.Request.Header.Set("X-Request", "two")
	clone.Request.Query.Set("q", "two")
	clone.Request.Body[0] = 'R'
	clone.Request.APISIXVars["$nested"].(map[string]any)["key"] = "two"
	clone.Request.RequestVars["$list"].([]any)[0] = "two"
	clone.Response.Header.Set("X-Response", "two")
	clone.Response.Trailer.Set("X-Trailer", "two")
	clone.Response.Body[0] = 'R'

	if original.Request.Header.Get("X-Request") != "one" || original.Request.Query.Get("q") != "one" ||
		string(original.Request.Body) != "request" ||
		original.Request.APISIXVars["$nested"].(map[string]any)["key"] != "one" ||
		original.Request.RequestVars["$list"].([]any)[0] != "one" ||
		original.Response.Header.Get("X-Response") != "one" ||
		original.Response.Trailer.Get("X-Trailer") != "one" || string(original.Response.Body) != "response" {
		t.Fatalf("source snapshot mutated through clone: %#v", original)
	}
}

func TestValueFromSnapshotCoversFallbackVariableSemantics(t *testing.T) {
	started := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	snapshot := LogSnapshot{
		Request: RequestLogSnapshot{
			Method:      http.MethodPatch,
			URI:         ":malformed?query",
			URL:         "/fallback?from=url",
			Host:        "plain-host",
			RemoteAddr:  "plain-remote",
			Scheme:      "http",
			Proto:       "HTTP/1.1",
			Header:      http.Header{"X-Multi": {"first", "second"}},
			Query:       url.Values{"fallback": {"query"}},
			Body:        []byte("request-body"),
			APISIXVars:  map[string]any{"$shared": "apisix", "$remote_addr": "effective"},
			RequestVars: map[string]any{"$shared": "request", "$request_only": "value"},
			Consumer:    SafeConsumerLogIdentity{Username: "alice", GroupID: "group-1"},
		},
		Response: ResponseLogSnapshot{Body: []byte("response-body")},
		Outcome:  ctx.ResponseOutcome{Status: http.StatusTeapot, Bytes: 13},
		Source:   ctx.ResponseSourceAPISIX,
		Started:  started,
	}
	want := map[string]any{
		"method": "PATCH", "request_uri": ":malformed?query", "uri": ":malformed?query",
		"host": "plain-host", "remote_addr": "effective", "remote_port": "",
		"args": "fallback=query", "scheme": "http", "proto": "HTTP/1.1",
		"status_code": http.StatusTeapot, "bytes_sent": int64(13),
		"request_body": "request-body", "response_body": "response-body",
		"consumer_name": "alice", "consumer_group_id": "group-1", "response_source": "apisix",
		"shared": "apisix", "request_only": "value", "arg_fallback": "query",
		"http_x_multi": "first", "unknown": "",
		"time_iso8601": started.Format(time.RFC3339),
	}
	for name, expected := range want {
		if got := ValueFromSnapshot(snapshot, name); !reflect.DeepEqual(got, expected) {
			t.Errorf("ValueFromSnapshot(%q) = %#v, want %#v", name, got, expected)
		}
	}
	if got := ValueFromSnapshot(
		LogSnapshot{Request: RequestLogSnapshot{URL: "/url-only"}},
		"request_uri",
	); got != "/url-only" {
		t.Fatalf("URL fallback request_uri = %#v", got)
	}
}

type snapshotErrorBody struct{}

type snapshotPanicBody struct{}

func (snapshotPanicBody) Read([]byte) (int, error) { panic("live request body read") }
func (snapshotPanicBody) Close() error             { return nil }

func (snapshotErrorBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (snapshotErrorBody) Close() error             { return nil }

func TestBuildSnapshotHandlesBodyAndSchemeBoundaries(t *testing.T) {
	if got := BuildSnapshot(nil, ResponseSnapshot{Body: []byte("response")}, ctx.ResponseOutcome{},
		ctx.ResponseSourceUnknown, time.Time{}, time.Time{}); string(got.Response.Body) != "response" {
		t.Fatalf("nil-request snapshot = %#v", got)
	}

	request := httptest.NewRequest(http.MethodPost, "https://gateway.test/upload", nil)
	request = ctx.WithRequestVars(request)
	ctx.RegisterRequestVar(request, ctx.RequestBodyKey, bytesOf('x', snapshotBodyLimit+1))
	request = ctx.WithApisixVars(request, map[string]string{
		"$consumer_name": "alice", "$consumer_group_id": "group-1",
	})
	ctx.RegisterApisixVar(request, "$consumer", map[string]any{"secret": "hidden"})
	snapshot := BuildSnapshot(request, ResponseSnapshot{}, ctx.ResponseOutcome{}, ctx.ResponseSourceUpstream,
		time.Now(), time.Now())
	if len(snapshot.Request.Body) != snapshotBodyLimit || !snapshot.Request.BodyTruncated {
		t.Fatalf("context body capture = %d/truncated=%v", len(snapshot.Request.Body), snapshot.Request.BodyTruncated)
	}
	if snapshot.Request.Consumer.Username != "alice" || snapshot.Request.Consumer.GroupID != "group-1" {
		t.Fatalf("safe consumer = %#v", snapshot.Request.Consumer)
	}
	if _, ok := snapshot.Request.APISIXVars["$consumer"]; ok {
		t.Fatal("unsafe consumer resource crossed snapshot boundary")
	}

	streamed := httptest.NewRequest(http.MethodPost, "http://gateway.test/upload", strings.NewReader("stream-body"))
	streamSnapshot := BuildSnapshot(streamed, ResponseSnapshot{}, ctx.ResponseOutcome{}, ctx.ResponseSourceUpstream,
		time.Now(), time.Now())
	replayed, err := io.ReadAll(streamed.Body)
	if err != nil || string(replayed) != "stream-body" || string(streamSnapshot.Request.Body) != "stream-body" {
		t.Fatalf("stream body snapshot/replay = %q/%q/%v", streamSnapshot.Request.Body, replayed, err)
	}

	errorRequest := httptest.NewRequest(http.MethodPost, "http://gateway.test/upload", nil)
	errorRequest.Body = snapshotErrorBody{}
	if body, truncated := captureRequestBody(errorRequest); body != nil || truncated {
		t.Fatalf("error body capture = %q/%v", body, truncated)
	}
	if _, err := io.ReadAll(errorRequest.Body); err == nil {
		t.Fatal("erroring body was not restored")
	}

	forwarded := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "wss")
	if got := requestScheme(forwarded); got != "wss" {
		t.Fatalf("forwarded scheme = %q", got)
	}
	forwarded.Header.Del("X-Forwarded-Proto")
	forwarded.URL.Scheme = "custom"
	if got := requestScheme(forwarded); got != "custom" {
		t.Fatalf("URL scheme = %q", got)
	}
	forwarded.URL.Scheme = ""
	forwarded.TLS = &tls.ConnectionState{}
	if got := requestScheme(forwarded); got != "https" {
		t.Fatalf("TLS scheme = %q", got)
	}
}

func bytesOf(value byte, count int) []byte {
	return []byte(strings.Repeat(string(value), count))
}
