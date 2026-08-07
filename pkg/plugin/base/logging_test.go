package base

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
)

func TestReadAndRestoreRequestBodyTruncatesOnlyReturnedValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abcdef"))

	body, err := ReadAndRestoreRequestBody(r, 4)
	if err != nil {
		t.Fatalf("ReadAndRestoreRequestBody() error = %v", err)
	}
	if body != "abcd" {
		t.Fatalf("returned body = %q, want abcd", body)
	}
	restored, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != "abcdef" {
		t.Fatalf("restored body = %q, want original body", restored)
	}
}

func TestReadRequestBodyRestoresRequestBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload"))

	body, err := ReadRequestBody(r)
	if err != nil {
		t.Fatalf("ReadRequestBody() error = %v", err)
	}
	if string(body) != "payload" {
		t.Fatalf("returned body = %q, want payload", body)
	}
	restored, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != "payload" {
		t.Fatalf("restored body = %q, want payload", restored)
	}
}

func TestReadSharedRequestBodyUsesCurrentBodyAfterEarlierCache(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("original"))
	r = apisixctx.WithRequestVars(r)
	if _, err := apisixctx.ReadRequestBody(r); err != nil {
		t.Fatalf("cache original request body: %v", err)
	}
	ReplaceRequestBody(r, []byte("rewritten"))

	body, err := ReadSharedRequestBody(r, 0)
	if err != nil {
		t.Fatalf("ReadSharedRequestBody() error = %v", err)
	}
	if body != "rewritten" {
		t.Fatalf("ReadSharedRequestBody() = %q, want rewritten", body)
	}
}

func TestReadSharedRequestBodyCachesLargestString(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abcdefgh"))

	short, err := ReadSharedRequestBody(r, 4)
	if err != nil {
		t.Fatalf("ReadSharedRequestBody() error = %v", err)
	}
	if short != "abcd" {
		t.Fatalf("short body = %q, want abcd", short)
	}
	long, err := ReadSharedRequestBody(r, 8)
	if err != nil {
		t.Fatalf("ReadSharedRequestBody() error = %v", err)
	}
	if long != "abcdefgh" {
		t.Fatalf("long body = %q, want abcdefgh", long)
	}
	if got, err := ReadSharedRequestBody(r, 4); err != nil || got != "abcd" {
		t.Fatalf("repeated short body = %q, %v; want abcd, nil", got, err)
	}

	capture, ok := r.Context().Value(sharedRequestBodyContextKey{}).(*sharedRequestBodyCapture)
	if !ok {
		t.Fatal("shared request body capture missing from request context")
	}
	if capture.bodyTextLen != 8 {
		t.Fatalf("cached body text length = %d, want 8", capture.bodyTextLen)
	}
}

func TestWriteJSONMessageWritesStatusAndEscapedMessage(t *testing.T) {
	rr := httptest.NewRecorder()

	WriteJSONMessage(rr, http.StatusBadRequest, "bad \"input\"")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	if got := rr.Body.String(); got != `{"message":"bad \"input\""}` {
		t.Fatalf("body = %q, want escaped JSON message", got)
	}
}

func TestRemoteIPStripsPortWhenPresent(t *testing.T) {
	if got := RemoteIP("192.0.2.10:8080"); got != "192.0.2.10" {
		t.Fatalf("RemoteIP() = %q, want 192.0.2.10", got)
	}
	if got := RemoteIP("192.0.2.10"); got != "192.0.2.10" {
		t.Fatalf("RemoteIP() without port = %q, want original address", got)
	}
}

func TestRequestVarFromNginxSupportsHeadersAndRemoteAddress(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.10:8080"
	r.Header.Set("X-Request-Id", "request-123")

	if got := RequestVarFromNginx(r, "$remote_addr"); got != "192.0.2.10" {
		t.Fatalf("remote_addr = %q, want 192.0.2.10", got)
	}
	if got := RequestVarFromNginx(r, "$http_x_request_id"); got != "request-123" {
		t.Fatalf("http_x_request_id = %q, want request-123", got)
	}
}

func TestRequestVarReadsApisixContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = apisixctx.WithApisixVars(r, map[string]string{"$route_id": "route-1"})

	if got := RequestVar(r, "$route_id", 0); got != "route-1" {
		t.Fatalf("RequestVar() = %q, want route-1", got)
	}
}

func TestSharedResponseRecorderReusesAdjacentLoggerWriter(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	outer := GetOrCreateSharedResponseRecorderWithLimit(httptest.NewRecorder(), r, 0)
	inner := GetOrCreateSharedResponseRecorderWithLimit(outer, r, 0)
	if inner != outer {
		t.Fatal("adjacent loggers should share one response recorder")
	}
}

func TestSharedResponseRecorderReusesCaptureThroughMetricsWrapper(t *testing.T) {
	destination := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	outer := GetOrCreateSharedResponseRecorderWithLimit(destination, r, 4)
	wrapped := httpsnoop.Wrap(outer, httpsnoop.Hooks{})
	inner := GetOrCreateSharedResponseRecorderWithLimit(wrapped, r, 8)

	_, _ = inner.Write([]byte("abcdefgh"))
	if got := outer.Body(); got != "abcdefgh" {
		t.Fatalf("outer captured body = %q, want shared eight-byte capture", got)
	}
	if got := inner.Body(); got != "abcdefgh" {
		t.Fatalf("inner captured body = %q, want shared eight-byte capture", got)
	}
}

func TestSharedResponseRecorderDoesNotBypassResponseTransformer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	destination := httptest.NewRecorder()
	outer := GetOrCreateSharedResponseRecorderWithLimit(destination, r, 0)
	transform := NewBufferedResponseWriter()
	inner := GetOrCreateSharedResponseRecorderWithLimit(transform, r, 0)
	if inner == outer {
		t.Fatal("logger separated by a response transformer must not reuse the outer recorder")
	}

	_, _ = inner.Write([]byte("upstream"))
	transform.SetBody([]byte("transformed"))
	transform.Commit(outer)

	if got := destination.Body.String(); got != "transformed" {
		t.Fatalf("destination body = %q, want transformed", got)
	}
	if got := outer.Body(); got != "transformed" {
		t.Fatalf("outer logger body = %q, want transformed", got)
	}
	if got := inner.Body(); got != "upstream" {
		t.Fatalf("inner logger body = %q, want upstream", got)
	}
}

func TestSharedResponseRecorderCaptureLimit(t *testing.T) {
	destination := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := GetOrCreateSharedResponseRecorderWithLimit(destination, r, 4)
	written, err := recorder.Write([]byte("abcdefgh"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if written != 8 || destination.Body.String() != "abcdefgh" {
		t.Fatalf("forwarded response = %q (%d bytes), want abcdefgh (8 bytes)", destination.Body.String(), written)
	}
	if recorder.Body() != "abcd" {
		t.Fatalf("captured response = %q, want abcd", recorder.Body())
	}
}

func TestSharedResponseRecorderCachesLargestBodyString(t *testing.T) {
	destination := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := GetOrCreateSharedResponseRecorderWithLimit(destination, r, 0)
	if _, err := recorder.Write([]byte("abcd")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := recorder.BodyTruncated(2); got != "ab" {
		t.Fatalf("short body = %q, want ab", got)
	}
	if _, err := recorder.Write([]byte("efgh")); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	if got := recorder.Body(); got != "abcdefgh" {
		t.Fatalf("full body = %q, want abcdefgh", got)
	}
	if got := recorder.BodyTruncated(4); got != "abcd" {
		t.Fatalf("repeated short body = %q, want abcd", got)
	}
	if got := recorder.sharedCapture().bodyTextLen; got != 8 {
		t.Fatalf("cached body text length = %d, want 8", got)
	}
}

func TestSharedResponseRecorderCachesDecodedBody(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte("hello world")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	destination := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := GetOrCreateSharedResponseRecorderWithLimit(destination, r, 0)
	if _, err := recorder.Write(compressed.Bytes()); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if got := recorder.BodyDecoded(5, " GZIP "); got != "hello" {
		t.Fatalf("short decoded body = %q, want hello", got)
	}
	if got := recorder.BodyDecoded(0, "gzip"); got != "hello world" {
		t.Fatalf("full decoded body = %q, want hello world", got)
	}
	capture := recorder.sharedCapture()
	if !capture.decodedReady || capture.decodedEncoding != "gzip" {
		t.Fatalf("decoded cache = ready:%v encoding:%q, want ready gzip", capture.decodedReady, capture.decodedEncoding)
	}
}

func TestResponseRecorderForwardsAndCapturesBoundedResponse(t *testing.T) {
	rr := httptest.NewRecorder()
	recorder := NewResponseRecorder(rr, 4)

	if _, err := recorder.Write([]byte("abcdef")); err != nil {
		t.Fatalf("ResponseRecorder.Write() error = %v", err)
	}
	if rr.Body.String() != "abcdef" {
		t.Fatalf("forwarded body = %q, want original body", rr.Body.String())
	}
	if recorder.Body() != "abcd" {
		t.Fatalf("captured body = %q, want bounded body", recorder.Body())
	}
	if recorder.StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.StatusCode())
	}
}

func TestExprMatchedSupportsBothPluginExpressionShapes(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/items?id=42", nil)
	r.Header.Set("X-Trace-Id", "abc-123")

	tests := []struct {
		name        string
		expressions any
	}{
		{
			name: "flat expressions",
			expressions: []any{
				[]any{"$arg_id", "==", "42"},
				"AND",
				[]any{"$http_x_trace_id", "~", "^abc"},
			},
		},
		{
			name: "nested expressions",
			expressions: [][]any{
				{"$arg_id", "==", "42"},
				{"AND"},
				{"$http_x_trace_id", "~", "^abc"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !ExprMatched(r, test.expressions, 0) {
				t.Fatalf("ExprMatched() = false, want true")
			}
		})
	}
}

func TestInitLogger(t *testing.T) {
	p := &BaseLoggerPlugin{}
	send := func(map[string]any) {}
	p.InitLogger(send)

	if p.FireChan == nil {
		t.Fatal("FireChan not initialized")
	}
	if !p.AsyncBlock {
		t.Fatal("AsyncBlock = false, want true")
	}
	if p.SendFunc == nil {
		t.Fatal("SendFunc not set")
	}
}

func TestApplyBatchDefaults(t *testing.T) {
	d := BatchDefaults{}
	ApplyBatchDefaults(&d)
	if d.BatchMaxSize != logger_batch.DefaultBatchMaxSize {
		t.Fatalf("BatchMaxSize = %d, want %d", d.BatchMaxSize, logger_batch.DefaultBatchMaxSize)
	}
	if d.RetryDelaySec != int(logger_batch.DefaultRetryDelay/time.Second) {
		t.Fatalf("RetryDelaySec = %d, want %d", d.RetryDelaySec, int(logger_batch.DefaultRetryDelay/time.Second))
	}
	if d.BufferDurationSec != int(logger_batch.DefaultBufferDuration/time.Second) {
		t.Fatalf(
			"BufferDurationSec = %d, want %d",
			d.BufferDurationSec,
			int(logger_batch.DefaultBufferDuration/time.Second),
		)
	}
	if d.InactiveTimeoutSec != int(logger_batch.DefaultInactiveTimeout/time.Second) {
		t.Fatalf(
			"InactiveTimeoutSec = %d, want %d",
			d.InactiveTimeoutSec,
			int(logger_batch.DefaultInactiveTimeout/time.Second),
		)
	}

	d = BatchDefaults{RetryDelaySec: 5, RetryDelaySet: false}
	ApplyBatchDefaults(&d)
	if d.RetryDelaySec != 5 {
		t.Fatalf("RetryDelaySec = %d, want 5 (explicit value preserved)", d.RetryDelaySec)
	}

	d = BatchDefaults{RetryDelaySet: true}
	ApplyBatchDefaults(&d)
	if d.RetryDelaySec != 0 {
		t.Fatalf("RetryDelaySec = %d, want 0 (set flag preserves zero)", d.RetryDelaySec)
	}

	d = BatchDefaults{BatchMaxSize: 42}
	ApplyBatchDefaults(&d)
	if d.BatchMaxSize != 42 {
		t.Fatalf("BatchMaxSize = %d, want 42 (explicit value preserved)", d.BatchMaxSize)
	}
}

func TestNewBatchProcessorDeliversPushedEntry(t *testing.T) {
	var delivered []map[string]any
	processor := NewBatchProcessor("test logger", BatchDefaults{}, "route-1", "server-1",
		func(entries []map[string]any, _ int) (int, error) {
			delivered = append(delivered, entries...)
			return 0, nil
		})

	if !processor.Push(map[string]any{"message": "hello"}) {
		t.Fatal("Push() = false, want true")
	}
	processor.Stop()

	if len(delivered) != 1 {
		t.Fatalf("delivered %d entries, want 1", len(delivered))
	}
	if got := delivered[0]["message"]; got != "hello" {
		t.Fatalf("delivered message = %v, want hello", got)
	}
}

func TestCompareNumber(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		op    string
		want  bool
	}{
		{name: "greater", left: "3", right: "2", op: ">", want: true},
		{name: "not greater", left: "2", right: "3", op: ">"},
		{name: "greater or equal", left: "3", right: "3", op: ">=", want: true},
		{name: "less", left: "2", right: "3", op: "<", want: true},
		{name: "less or equal", left: "3", right: "3", op: "<=", want: true},
		{name: "invalid left", left: "x", right: "3", op: ">"},
		{name: "invalid right", left: "3", right: "x", op: ">"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := compareNumber(test.left, test.right, func(a, b float64) bool {
				switch test.op {
				case ">":
					return a > b
				case ">=":
					return a >= b
				case "<":
					return a < b
				default:
					return a <= b
				}
			})
			if got != test.want {
				t.Fatalf("compareNumber(%q, %q) = %t, want %t", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestNestedLogMapReturnsExistingOrCreates(t *testing.T) {
	fields := map[string]any{"existing": map[string]any{"k": "v"}}
	if got := NestedLogMap(fields, "existing"); got["k"] != "v" {
		t.Fatalf("NestedLogMap(existing) = %v, want existing map", got)
	}
	created := NestedLogMap(fields, "created")
	created["new"] = 1
	if fields["created"].(map[string]any)["new"] != 1 {
		t.Fatal("NestedLogMap() did not install the created map into fields")
	}
}

func TestRequestVarResolvesExpressionVariables(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://api.example.test:8443/orders?q=blue", nil)
	request.RemoteAddr = "192.0.2.20:1234"
	request.Header.Set("X-Custom", "custom-value")

	if got := RequestVar(request, "$status", 201); got != "201" {
		t.Fatalf("status = %q, want 201", got)
	}
	if got := RequestVar(request, "$status_code", 0); got != "<nil>" {
		t.Fatalf("status_code without context = %q, want <nil>", got)
	}
	if got := RequestVar(request, "$uri", 0); got != "/orders" {
		t.Fatalf("uri = %q, want /orders", got)
	}
	if got := RequestVar(request, "$request_uri", 0); got != "/orders?q=blue" {
		t.Fatalf("request_uri = %q, want /orders?q=blue", got)
	}
	if got := RequestVar(request, "$method", 0); got != http.MethodPost {
		t.Fatalf("method = %q, want POST", got)
	}
	if got := RequestVar(request, "$host", 0); got != "api.example.test:8443" {
		t.Fatalf("host = %q", got)
	}
	if got := RequestVar(request, "$scheme", 0); got != "https" {
		t.Fatalf("scheme = %q, want https", got)
	}
	request.Header.Set("X-Forwarded-Proto", "http")
	if got := RequestVar(request, "$scheme", 0); got != "http" {
		t.Fatalf("scheme with X-Forwarded-Proto = %q, want http", got)
	}
	if got := RequestVar(request, "$remote_addr", 0); got != "192.0.2.20" {
		t.Fatalf("remote_addr = %q, want 192.0.2.20", got)
	}
	if got := RequestVar(request, "$arg_q", 0); got != "blue" {
		t.Fatalf("arg_q = %q, want blue", got)
	}
	if got := RequestVar(request, "$http_x_custom", 0); got != "custom-value" {
		t.Fatalf("http_x_custom = %q, want custom-value", got)
	}
	if got := RequestVar(request, "$unknown_var", 0); got != "" {
		t.Fatalf("unknown var = %q, want empty", got)
	}
}

func TestResponseRecorderStatusAndBodyAccessors(t *testing.T) {
	recorder := NewResponseRecorder(httptest.NewRecorder(), 1024)
	if recorder.StatusCode() != 0 || recorder.HasBody() {
		t.Fatal("fresh recorder reports status or body")
	}
	recorder.WriteHeader(http.StatusAccepted)
	if recorder.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.StatusCode())
	}
	_, _ = recorder.Write([]byte("payload"))
	if !recorder.HasBody() || recorder.Body() != "payload" {
		t.Fatalf("body = %q/%t, want payload/true", recorder.Body(), recorder.HasBody())
	}
}

func TestDecodeResponseBody(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte("gzip-payload"))
	_ = writer.Close()

	if got := decodeResponseBody(compressed.Bytes(), "gzip"); got != "gzip-payload" {
		t.Fatalf("gzip decode = %q, want gzip-payload", got)
	}
	if got := decodeResponseBody(compressed.Bytes(), "unknown"); got != compressed.String() {
		t.Fatal("unknown encoding must pass through unchanged")
	}
	if got := decodeResponseBody([]byte("not gzip"), "gzip"); got != "not gzip" {
		t.Fatalf("invalid gzip = %q, want raw bytes", got)
	}
	if got := decodeResponseBody([]byte("plain"), "identity"); got != "plain" {
		t.Fatalf("identity decode = %q, want plain", got)
	}
}

func TestResponseWriterWrapsSharedCapture(t *testing.T) {
	recorder := NewSharedResponseRecorder(httptest.NewRecorder())
	_, _ = recorder.Write([]byte("captured"))

	direct := &SharedResponseRecorder{
		ResponseWriter: recorder,
		capture:        recorder.sharedCapture(),
		forwardOnly:    true,
	}
	if !responseWriterWrapsSharedCapture(direct, recorder.sharedCapture()) {
		t.Fatal("responseWriterWrapsSharedCapture() = false for a direct wrapper")
	}
	unwrapped := &wrappedWriter{ResponseWriter: recorder}
	if !responseWriterWrapsSharedCapture(unwrapped, recorder.sharedCapture()) {
		t.Fatal("responseWriterWrapsSharedCapture() = false through an unwrap chain")
	}
	if responseWriterWrapsSharedCapture(httptest.NewRecorder(), recorder.sharedCapture()) {
		t.Fatal("responseWriterWrapsSharedCapture() = true for an unrelated writer")
	}
}

type wrappedWriter struct {
	http.ResponseWriter
}

func (w *wrappedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestSharedResponseRecorderBodyAccessors(t *testing.T) {
	recorder := NewSharedResponseRecorder(httptest.NewRecorder())
	if recorder.HasBody() || len(recorder.BodyBytes()) != 0 {
		t.Fatal("fresh recorder reports a body")
	}
	recorder.WriteHeader(http.StatusCreated)
	_, _ = recorder.Write([]byte("payload"))

	if recorder.StatusCode() != http.StatusCreated {
		t.Fatalf("status = %d, want first status 201", recorder.StatusCode())
	}
	if !recorder.HasBody() || recorder.Body() != "payload" {
		t.Fatalf("body = %q/%t", recorder.Body(), recorder.HasBody())
	}
	if got := recorder.BodyTruncated(3); got != "pay" {
		t.Fatalf("BodyTruncated(3) = %q, want pay", got)
	}
	if got := recorder.BodyDecoded(0, "identity"); got != "payload" {
		t.Fatalf("BodyDecoded(identity) = %q, want payload", got)
	}
	if got := recorder.BodyDecoded(0, ""); got != "payload" {
		t.Fatalf("BodyDecoded(empty) = %q, want payload", got)
	}
}

func TestUpdateSharedResponseCaptureLimit(t *testing.T) {
	capture := &sharedResponseCapture{maxBytes: 100}
	updateSharedResponseCaptureLimit(capture, 50)
	if capture.maxBytes != 100 {
		t.Fatalf("maxBytes = %d, want existing larger limit kept", capture.maxBytes)
	}
	updateSharedResponseCaptureLimit(capture, 200)
	if capture.maxBytes != 200 {
		t.Fatalf("maxBytes = %d, want larger limit adopted", capture.maxBytes)
	}
	updateSharedResponseCaptureLimit(capture, 0)
	if capture.maxBytes != 0 {
		t.Fatalf("maxBytes = %d, want unlimited when a limit is zero", capture.maxBytes)
	}
}
