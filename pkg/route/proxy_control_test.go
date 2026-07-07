package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
)

func TestBufferRequestBodyIfNeededBuffersWhenEnabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/upload", &countingReadCloser{
		Reader: strings.NewReader("upload-body"),
	})
	req.ContentLength = int64(len("upload-body"))
	req = proxy_control.WithRequestBuffering(req, true)

	if err := bufferRequestBodyIfNeeded(req); err != nil {
		t.Fatalf("bufferRequestBodyIfNeeded() error = %v", err)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read buffered body: %v", err)
	}
	if string(body) != "upload-body" {
		t.Fatalf("body = %q, want upload-body", body)
	}
	if req.GetBody == nil {
		t.Fatal("GetBody is nil, want replayable buffered body")
	}

	replayed, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody() error = %v", err)
	}
	replayedBody, err := io.ReadAll(replayed)
	if err != nil {
		t.Fatalf("read replayed body: %v", err)
	}
	if string(replayedBody) != "upload-body" {
		t.Fatalf("replayed body = %q, want upload-body", replayedBody)
	}
}

func TestBufferRequestBodyIfNeededSkipsWhenDisabled(t *testing.T) {
	original := &countingReadCloser{Reader: strings.NewReader("stream")}
	req := httptest.NewRequest(http.MethodPost, "/upload", original)
	req = proxy_control.WithRequestBuffering(req, false)

	if err := bufferRequestBodyIfNeeded(req); err != nil {
		t.Fatalf("bufferRequestBodyIfNeeded() error = %v", err)
	}

	if req.Body != original {
		t.Fatal("request body was replaced, want original streaming body")
	}
	if original.reads != 0 {
		t.Fatalf("body reads = %d, want 0", original.reads)
	}
}

func TestSelectProxyHandlerUsesStreamingHandlerWhenProxyBufferingDisabled(t *testing.T) {
	defaultCalled := false
	streamingCalled := false
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	req = proxy_buffering.WithDisableProxyBuffering(req, true)

	handler := selectProxyHandler(
		req,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defaultCalled = true
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamingCalled = true
		}),
	)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if defaultCalled {
		t.Fatal("default proxy handler was called, want streaming handler")
	}
	if !streamingCalled {
		t.Fatal("streaming proxy handler was not called")
	}
}

func TestModifyResponseRecordsUpstreamLatency(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, upstreamStartTimeVar, time.Now().Add(-25*time.Millisecond))

	resp := &http.Response{
		StatusCode: http.StatusAccepted,
		Request:    req,
	}

	if err := newModifyResponse()(resp); err != nil {
		t.Fatalf("modify response error = %v", err)
	}

	if got := apisixctx.GetRequestVar(req, "$status"); got != http.StatusAccepted {
		t.Fatalf("$status = %v, want %d", got, http.StatusAccepted)
	}
	latency, ok := apisixctx.GetRequestVar(req, upstreamLatencyVar).(int64)
	if !ok {
		t.Fatalf("%s was not recorded as int64", upstreamLatencyVar)
	}
	if latency <= 0 {
		t.Fatalf("%s = %d, want positive latency", upstreamLatencyVar, latency)
	}
}

type countingReadCloser struct {
	*strings.Reader
	reads int
}

func (b *countingReadCloser) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *countingReadCloser) Close() error {
	return nil
}
