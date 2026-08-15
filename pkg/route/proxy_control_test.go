package route

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestBufferRequestBodyIfNeededBuffersWhenEnabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/upload", &countingReadCloser{
		Reader: strings.NewReader("upload-body"),
	})
	req.ContentLength = int64(len("upload-body"))
	req = proxy_control.WithRequestBuffering(req, true)

	if err := bufferRequestBodyIfNeeded(httptest.NewRecorder(), req); err != nil {
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

	if err := bufferRequestBodyIfNeeded(httptest.NewRecorder(), req); err != nil {
		t.Fatalf("bufferRequestBodyIfNeeded() error = %v", err)
	}

	if req.Body != original {
		t.Fatal("request body was replaced, want original streaming body")
	}
	if original.reads != 0 {
		t.Fatalf("body reads = %d, want 0", original.reads)
	}
}

func TestBufferRequestBodyIfNeededRejectsBodyAboveReplayLimit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), int(proxy_control.DefaultRequestBufferingLimit+1))
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test/upload", bytes.NewReader(body))
	request = proxy_control.WithRequestBuffering(request, true)
	recorder := httptest.NewRecorder()
	err := bufferRequestBodyIfNeeded(recorder, request)
	var maxBytesErr *http.MaxBytesError
	if !errors.As(err, &maxBytesErr) {
		t.Fatalf("bufferRequestBodyIfNeeded() error = %v, want *http.MaxBytesError", err)
	}
}

func TestProxyHandlerRejectsOversizedBufferedRequestWith413(t *testing.T) {
	handler, err := (&Builder{}).buildReverseHandler(resource.Route{
		Upstream: resource.Upstream{
			Scheme: "http",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}

	body := bytes.Repeat([]byte("x"), int(proxy_control.DefaultRequestBufferingLimit+1))
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test/upload", bytes.NewReader(body))
	request = proxy_control.WithRequestBuffering(request, true)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
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

func TestHTTPRetryCountDefaultsToRemainingUpstreamNodes(t *testing.T) {
	upstream := resource.Upstream{
		Nodes: []resource.Node{{}, {}, {}},
	}
	if got := httpRetryCount(upstream); got != 2 {
		t.Fatalf("httpRetryCount() = %d, want 2 for three nodes", got)
	}

	if err := json.Unmarshal([]byte(`{
		"nodes": {
			"127.0.0.1:8080": 1,
			"127.0.0.2:8080": 1,
			"127.0.0.3:8080": 1
		},
		"retries": 0
	}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got := httpRetryCount(upstream); got != 0 {
		t.Fatalf("httpRetryCount() = %d, want explicit zero", got)
	}

	upstream.Retries = 1
	if got := httpRetryCount(upstream); got != 1 {
		t.Fatalf("httpRetryCount() = %d, want explicit one", got)
	}
}

func TestAttachHTTPRetriesAdvancesTrafficSplitTargets(t *testing.T) {
	retryHosts := []string{"127.0.0.2:8080", "127.0.0.3:8080"}
	next := 0
	override := &traffic_split.Override{
		Scheme:   "http",
		Host:     "127.0.0.1:8080",
		PassHost: "node",
		Retries:  2,
		NextRetry: func() *traffic_split.Override {
			if next >= len(retryHosts) {
				return nil
			}
			selected := &traffic_split.Override{
				Scheme:   "http",
				Host:     retryHosts[next],
				PassHost: "node",
			}
			next++
			return selected
		},
	}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	request = traffic_split.WithOverride(request, override)
	if !applyTrafficSplitOverride(request) {
		t.Fatal("initial traffic-split override was not applied")
	}
	request = attachHTTPRetriesCompiled(request, resource.Upstream{}, nil, nil)

	var attemptedHosts []string
	transport := pxy.NewRetryTransport(routeRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		attemptedHosts = append(attemptedHosts, request.URL.Host)
		if len(attemptedHosts) < 3 {
			return nil, errors.New("connection refused")
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       http.NoBody,
			Request:    request,
		}, nil
	}))
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	want := []string{"127.0.0.1:8080", "127.0.0.2:8080", "127.0.0.3:8080"}
	if strings.Join(attemptedHosts, ",") != strings.Join(want, ",") {
		t.Fatalf("attempted hosts = %v, want %v", attemptedHosts, want)
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
	if got := apisixctx.GetRequestVar(req, "$upstream_status"); got != http.StatusAccepted {
		t.Fatalf("$upstream_status = %v, want %d", got, http.StatusAccepted)
	}
	if got := apisixctx.GetRequestVar(req, "$response_source"); got != "upstream" {
		t.Fatalf("$response_source = %v, want upstream", got)
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

type routeRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f routeRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
