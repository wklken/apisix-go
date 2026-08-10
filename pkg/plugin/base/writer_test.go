package base

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type commitRecorder struct {
	header   http.Header
	statuses []int
	headers  []http.Header
	body     bytes.Buffer
}

func newCommitRecorder() *commitRecorder {
	return &commitRecorder{header: make(http.Header)}
}

func (r *commitRecorder) Header() http.Header {
	return r.header
}

func (r *commitRecorder) WriteHeader(statusCode int) {
	r.statuses = append(r.statuses, statusCode)
	r.headers = append(r.headers, r.header.Clone())
}

func (r *commitRecorder) Write(body []byte) (int, error) {
	if len(r.statuses) == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

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

func TestBufferedResponseWriterReplaceBodyInvalidatesHeaders(t *testing.T) {
	w := NewBufferedResponseWriter()
	w.Header().Set("Content-Length", "8")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Range", "bytes 0-7/8")
	w.Header().Set("Content-MD5", "md5")
	w.Header().Set("Digest", "sha-256=x")
	w.Header().Set("Content-Digest", "sha-256=:x:")
	w.Header().Set("Repr-Digest", "sha-256=:x:")
	w.Header().Set("ETag", `"upstream"`)
	w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
	w.Header().Set("Content-Type", "text/plain")
	w.ReplaceBody([]byte("rewritten"))

	for _, field := range []string{
		"Content-Length", "Content-Encoding", "Content-Range", "Content-MD5",
		"Digest", "Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
	} {
		if got := w.Header().Values(field); len(got) != 0 {
			t.Errorf("%s = %v, want removed", field, got)
		}
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want preserved", got)
	}
	if got := string(w.Body()); got != "rewritten" {
		t.Fatalf("Body() = %q, want rewritten", got)
	}
}

func TestBufferedResponseWriterReplaceBodyPreservesSharedBuffer(t *testing.T) {
	req := newPipelineRequest("/test", 2)
	w1 := GetOrCreateTransformResponseWriter(req)
	w2 := GetOrCreateTransformResponseWriter(req)
	_, _ = w1.Write([]byte("original"))
	w2.ReplaceBody([]byte("rewritten"))

	if w2.bodyPtr == nil || w1.bodyPtr != w2.bodyPtr {
		t.Fatal("ReplaceBody must retain the shared pipeline buffer")
	}
	if got := string(w1.Body()); got != "rewritten" {
		t.Fatalf("shared Body() = %q, want rewritten", got)
	}
}

func TestBufferedResponseWriterCommitReplaysToDestination(t *testing.T) {
	w := NewBufferedResponseWriter()
	w.Header().Set("X-A", "1")
	w.Header().Add("X-B", "2")
	w.Header().Add("X-B", "3")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("payload"))

	rr := httptest.NewRecorder()
	w.Commit(rr)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("committed status = %d, want 202", rr.Code)
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

func TestBufferedResponseWriterCommitReplaysInformationalBeforeFinal(t *testing.T) {
	w := NewBufferedResponseWriter()
	w.Header().Set("Link", "</early>; rel=preload")
	w.WriteHeader(http.StatusEarlyHints)
	w.Header().Set("Link", "</final>")
	w.Header().Set("X-Final", "yes")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("payload"))

	dst := newCommitRecorder()
	w.Commit(dst)

	if got, want := dst.statuses, []int{http.StatusEarlyHints, http.StatusOK}; !equalInts(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if got := dst.headers[0].Get("Link"); got != "</early>; rel=preload" {
		t.Fatalf("informational Link = %q, want early link", got)
	}
	if got := dst.headers[1].Get("Link"); got != "</final>" {
		t.Fatalf("final Link = %q, want final link", got)
	}
	if got := dst.headers[1].Get("X-Final"); got != "yes" {
		t.Fatalf("final X-Final = %q, want yes", got)
	}
	if got := dst.body.String(); got != "payload" {
		t.Fatalf("body = %q, want payload", got)
	}
}

func TestBufferedResponseWriterCommitSuppressesBodyForBodylessStatuses(t *testing.T) {
	for _, status := range []int{http.StatusSwitchingProtocols, http.StatusNoContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			w := NewBufferedResponseWriter()
			w.Header().Set("Content-Length", "7")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("payload"))

			dst := newCommitRecorder()
			w.Commit(dst)

			if got, want := dst.statuses, []int{status}; !equalInts(got, want) {
				t.Fatalf("statuses = %v, want %v", got, want)
			}
			if got := dst.body.String(); got != "" {
				t.Fatalf("body = %q, want empty for %d", got, status)
			}
			if got := dst.headers[0].Get("Content-Length"); got != "" {
				t.Fatalf("Content-Length = %q, want removed for %d", got, status)
			}
		})
	}
}

func TestBufferedResponseWriterWriteRejectsBodyForFinalBodylessStatus(t *testing.T) {
	for _, status := range []int{http.StatusSwitchingProtocols, http.StatusNoContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			w := GetOrCreateTransformResponseWriter(req)
			w.WriteHeader(status)

			n, err := w.Write([]byte("payload"))
			if n != 0 {
				t.Fatalf("Write() n = %d, want 0 for %d", n, status)
			}
			if err != http.ErrBodyNotAllowed {
				t.Fatalf("Write() error = %v, want %v for %d", err, http.ErrBodyNotAllowed, status)
			}

			dst := newCommitRecorder()
			w.Commit(dst)
			if got := dst.body.String(); got != "" {
				t.Fatalf("committed body = %q, want empty for %d", got, status)
			}
		})
	}
}

func TestBufferedResponseWriterHeadWriteReportsLengthWithoutCommit(t *testing.T) {
	req := httptest.NewRequest(http.MethodHead, "/test", nil)
	w := GetOrCreateTransformResponseWriter(req)
	w.WriteHeader(http.StatusOK)

	payload := []byte("payload")
	n, err := w.Write(payload)
	if n != len(payload) {
		t.Fatalf("HEAD Write() n = %d, want %d", n, len(payload))
	}
	if err != nil {
		t.Fatalf("HEAD Write() error = %v, want nil", err)
	}

	dst := newCommitRecorder()
	w.Commit(dst)
	if got := dst.body.String(); got != "" {
		t.Fatalf("HEAD committed body = %q, want empty", got)
	}
}

func TestBufferedResponseWriterSetStatusCodeSuppressesRemappedBody(t *testing.T) {
	w := NewBufferedResponseWriter()
	w.Header().Set("Content-Length", "7")
	_, _ = w.Write([]byte("payload"))
	w.SetStatusCode(http.StatusNoContent)

	dst := newCommitRecorder()
	w.Commit(dst)

	if got, want := dst.statuses, []int{http.StatusNoContent}; !equalInts(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if got := dst.body.String(); got != "" {
		t.Fatalf("body = %q, want empty after 200 -> 204", got)
	}
	if got := dst.headers[0].Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after 200 -> 204", got)
	}
}

func TestTransformResponseWriterSuppressesBodyForNestedHead(t *testing.T) {
	req := newPipelineRequest("/test", 2)
	req.Method = http.MethodHead
	inner := GetOrCreateTransformResponseWriter(req)
	outer := GetOrCreateTransformResponseWriter(req)
	inner.WriteHeader(http.StatusOK)
	_, _ = inner.Write([]byte("payload"))
	inner.Commit(outer)

	dst := newCommitRecorder()
	outer.Commit(dst)

	if got, want := dst.statuses, []int{http.StatusOK}; !equalInts(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if got := dst.body.String(); got != "" {
		t.Fatalf("HEAD body = %q, want empty", got)
	}
}

func TestTransformResponseWriterRecordsStandaloneHead(t *testing.T) {
	req := httptest.NewRequest(http.MethodHead, "/test", nil)
	writer := GetOrCreateTransformResponseWriter(req)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("payload"))

	dst := newCommitRecorder()
	writer.Commit(dst)

	if got := dst.body.String(); got != "" {
		t.Fatalf("standalone HEAD body = %q, want empty", got)
	}
}

func TestTransformResponseWriterPropagatesNestedNoBodyStatus(t *testing.T) {
	req := newPipelineRequest("/test", 2)
	inner := GetOrCreateTransformResponseWriter(req)
	outer := GetOrCreateTransformResponseWriter(req)
	inner.WriteHeader(http.StatusNoContent)
	_, _ = inner.Write([]byte("payload"))
	inner.Commit(outer)

	dst := newCommitRecorder()
	outer.Commit(dst)

	if got, want := dst.statuses, []int{http.StatusNoContent}; !equalInts(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if got := dst.body.String(); got != "" {
		t.Fatalf("nested 204 body = %q, want empty", got)
	}
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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
