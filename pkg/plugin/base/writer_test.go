package base

import (
	"bytes"
	"context"
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

func newPipelineRequest(target string, transformCount int) *http.Request {
	req, _ := http.NewRequest("GET", target, nil)
	pipeline := &pipelineBuffer{count: transformCount}
	return req.WithContext(context.WithValue(req.Context(), transformPipelineContextKey{}, pipeline))
}

func TestSingleTransformUsesStandaloneBuffer(t *testing.T) {
	req := newPipelineRequest("/test", 1)
	writer := GetOrCreateTransformResponseWriter(req)
	if writer.bodyPtr != nil {
		t.Fatal("single transform should use standalone buffering")
	}
}

func TestPipelineBodySharing(t *testing.T) {
	// Simulate two transform plugins sharing a pipeline buffer.
	req := newPipelineRequest("/test", 2)

	w1 := GetOrCreateTransformResponseWriter(req)
	w2 := GetOrCreateTransformResponseWriter(req)

	// w1 and w2 should share the same underlying buffer.
	if got := w1.bodyPtr; got == nil {
		t.Fatal("w1.bodyPtr should not be nil in pipeline mode")
	}
	if w1.bodyPtr != w2.bodyPtr {
		t.Fatal("w1 and w2 should share the same pipeline buffer")
	}

	// Write through w1, verify w2 sees it.
	_, _ = w1.Write([]byte("shared-body"))
	if got := string(w2.Body()); got != "shared-body" {
		t.Fatalf("w2.Body() = %q, want shared-body", got)
	}

	// SetBody through w2, verify w1 sees it.
	w2.SetBody([]byte("modified"))
	if got := string(w1.Body()); got != "modified" {
		t.Fatalf("w1.Body() after w2.SetBody = %q, want modified", got)
	}
}

func TestPipelineCommitSkipsBodyBetweenPipelines(t *testing.T) {
	req := newPipelineRequest("/test", 2)

	w1 := GetOrCreateTransformResponseWriter(req)
	w2 := GetOrCreateTransformResponseWriter(req)

	// Inner plugin (w2) writes body, sets headers, then commits to w1.
	w2.WriteHeader(http.StatusCreated)
	_, _ = w2.Write([]byte("from-inner"))
	w2.Header().Set("X-Inner", "true")
	w2.Commit(w1)

	// w1 should have w2's headers committed.
	if got := w1.Header().Get("X-Inner"); got != "true" {
		t.Fatalf("w1 X-Inner = %q, want true", got)
	}

	// Body should still be present (not duplicated).
	if got := string(w1.Body()); got != "from-inner" {
		t.Fatalf("w1.Body() = %q, want from-inner (not duplicated)", got)
	}

	// w1 modifies body and commits to real ResponseWriter.
	w1.SetBody([]byte("outer-modified"))
	w1.Header().Set("X-Outer", "yes")

	rr := httptest.NewRecorder()
	w1.Commit(rr)

	if rr.Code != http.StatusCreated {
		t.Fatalf("rr.Code = %d, want 201", rr.Code)
	}
	if got := rr.Body.String(); got != "outer-modified" {
		t.Fatalf("rr.Body = %q, want outer-modified", got)
	}
	if got := rr.Header().Get("X-Inner"); got != "true" {
		t.Fatalf("rr X-Inner = %q, want true", got)
	}
	if got := rr.Header().Get("X-Outer"); got != "yes" {
		t.Fatalf("rr X-Outer = %q, want yes", got)
	}

	// Verify w1's body buffer is not corrupted.
	if got := string(w1.Body()); got != "outer-modified" {
		t.Fatalf("w1.Body() after Commit = %q, want outer-modified", got)
	}
}

func TestPipelineSeparateHeaders(t *testing.T) {
	// Each pipeline writer has its own header map.
	req := newPipelineRequest("/test", 2)

	w1 := GetOrCreateTransformResponseWriter(req)
	w2 := GetOrCreateTransformResponseWriter(req)

	w2.Header().Set("X-A", "from-w2")
	w1.Header().Set("X-B", "from-w1")

	// Headers should be separate.
	if got := w1.Header().Get("X-A"); got != "" {
		t.Fatalf("w1 X-A = %q, want empty (separate headers)", got)
	}
	if got := w2.Header().Get("X-B"); got != "" {
		t.Fatalf("w2 X-B = %q, want empty (separate headers)", got)
	}

	// Commit w2 to w1 copies headers.
	w2.Commit(w1)
	if got := w1.Header().Get("X-A"); got != "from-w2" {
		t.Fatalf("w1 X-A after commit = %q, want from-w2", got)
	}
}

func TestPipelineEnsureBufferNotCopied(t *testing.T) {
	// Verify that committing between pipeline writers does not copy body bytes.
	req := newPipelineRequest("/test", 2)

	w1 := GetOrCreateTransformResponseWriter(req)
	w2 := GetOrCreateTransformResponseWriter(req)

	// Write large body to simulate real usage.
	largeBody := bytes.Repeat([]byte("x"), 10*1024) // 10 KiB
	_, _ = w2.Write(largeBody)

	// Snapshot w1's buffer pointer before commit.
	bufBefore := w1.bodyPtr

	w2.Commit(w1)

	// Buffer pointer should be unchanged (still same shared buffer).
	if w1.bodyPtr != bufBefore {
		t.Fatal("Pipeline buffer pointer changed after commit")
	}

	// Body content should be exactly the same (no duplication from Commit).
	if !bytes.Equal(w1.Body(), largeBody) {
		t.Fatalf("w1.Body() length = %d, want %d (body should not be duplicated)",
			len(w1.Body()), len(largeBody))
	}
}

func TestGetOrCreateTransformResponseWriterReturnsNewWithoutPipeline(t *testing.T) {
	// Without a pipeline in context, each call should create a NEW writer
	// with its own bodyPtr (but all sharing the pipeline buffer).
	req1 := newPipelineRequest("/a", 2)
	req2 := newPipelineRequest("/b", 2)

	w1 := GetOrCreateTransformResponseWriter(req1)
	w2 := GetOrCreateTransformResponseWriter(req2)

	// Different requests should have different pipeline buffers.
	if w1.bodyPtr == w2.bodyPtr {
		t.Fatal("different requests should not share pipeline buffers")
	}
}
