package base

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	outer := GetOrCreateSharedResponseRecorder(httptest.NewRecorder(), r)
	inner := GetOrCreateSharedResponseRecorder(outer, r)
	if inner != outer {
		t.Fatal("adjacent loggers should share one response recorder")
	}
}

func TestSharedResponseRecorderDoesNotBypassResponseTransformer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	destination := httptest.NewRecorder()
	outer := GetOrCreateSharedResponseRecorder(destination, r)
	transform := NewBufferedResponseWriter()
	inner := GetOrCreateSharedResponseRecorder(transform, r)
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
