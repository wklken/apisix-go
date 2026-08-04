package base

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBufferedResponseWriterCapturesBeforeCommit(t *testing.T) {
	w := NewBufferedResponseWriter()

	if got := w.StatusCode(); got != http.StatusOK {
		t.Fatalf("StatusCode() = %d, want default 200", got)
	}
	w.Header().Set("X-Custom", "value")
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := w.Write([]byte(" world")); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}

	if got := w.StatusCode(); got != http.StatusCreated {
		t.Fatalf("StatusCode() = %d, want 201", got)
	}
	if got := string(w.Body()); got != "hello world" {
		t.Fatalf("Body() = %q, want hello world", got)
	}
}

func TestBufferedResponseWriterFirstWriteHeaderWins(t *testing.T) {
	w := NewBufferedResponseWriter()
	w.WriteHeader(http.StatusAccepted)
	w.WriteHeader(http.StatusInternalServerError)

	if got := w.StatusCode(); got != http.StatusAccepted {
		t.Fatalf("StatusCode() = %d, want 202 (first WriteHeader wins)", got)
	}
}

func TestBufferedResponseWriterWriteDefaultsToOK(t *testing.T) {
	w := NewBufferedResponseWriter()
	if _, err := w.Write([]byte("body")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := w.StatusCode(); got != http.StatusOK {
		t.Fatalf("StatusCode() = %d, want 200", got)
	}
}

func TestBufferedResponseWriterSetBodyReplacesContent(t *testing.T) {
	w := NewBufferedResponseWriter()
	_, _ = w.Write([]byte("original"))
	w.SetBody([]byte("rewritten"))

	if got := string(w.Body()); got != "rewritten" {
		t.Fatalf("Body() after SetBody = %q, want rewritten", got)
	}
}

func TestBufferedResponseWriterCommitReplaysToDestination(t *testing.T) {
	w := NewBufferedResponseWriter()
	w.Header().Set("X-A", "1")
	w.Header().Add("X-B", "2")
	w.Header().Add("X-B", "3")
	w.WriteHeader(http.StatusNoContent)
	_, _ = w.Write([]byte("payload"))

	rr := httptest.NewRecorder()
	w.Commit(rr)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("committed status = %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("X-A"); got != "1" {
		t.Fatalf("committed X-A = %q, want 1", got)
	}
	if got := rr.Header().Values("X-B"); len(got) != 2 || got[0] != "2" || got[1] != "3" {
		t.Fatalf("committed X-B = %v, want [2 3]", got)
	}
	if got := rr.Body.String(); got != "payload" {
		t.Fatalf("committed body = %q, want payload", got)
	}
}
