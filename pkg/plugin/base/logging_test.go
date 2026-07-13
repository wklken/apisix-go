package base

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
