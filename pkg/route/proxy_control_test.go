package route

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

	cleanup, err := bufferRequestBodyIfNeeded(req)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
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

	cleanup, err := bufferRequestBodyIfNeeded(req)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("bufferRequestBodyIfNeeded() error = %v", err)
	}

	if req.Body != original {
		t.Fatal("request body was replaced, want original streaming body")
	}
	if original.reads != 0 {
		t.Fatalf("body reads = %d, want 0", original.reads)
	}
}

func TestBufferRequestBodyPreservesConfiguredClientLimit(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("too big"))
	request.Body = http.MaxBytesReader(httptest.NewRecorder(), request.Body, 3)
	cleanup, err := bufferRequestBodyIfNeeded(request)
	if cleanup != nil {
		defer cleanup()
	}
	var maxBytesErr *http.MaxBytesError
	if !errors.As(err, &maxBytesErr) {
		t.Fatalf("error=%v want configured body limit rejection", err)
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
		NextRetry: func(*http.Request) *traffic_split.Override {
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

func TestAttachHTTPRetriesDoesNotDoubleReportUnavailableTrafficSplitTarget(t *testing.T) {
	for _, test := range []struct {
		name      string
		nextRetry func(*http.Request) *traffic_split.Override
	}{
		{name: "nil callback"},
		{name: "nil result", nextRetry: func(*http.Request) *traffic_split.Override { return nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter := &routeHealthRecorder{}
			override := &traffic_split.Override{
				Scheme:         "http",
				Host:           "127.0.0.1:8080",
				Retries:        1,
				HealthReporter: reporter,
				HealthTarget:   "http://127.0.0.1:8080",
				NextRetry:      test.nextRetry,
			}
			request := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
			request = traffic_split.WithOverride(request, override)
			if !applyTrafficSplitOverride(request) {
				t.Fatal("initial traffic-split override was not applied")
			}
			request = attachHTTPRetriesCompiled(request, resource.Upstream{}, nil, nil)

			transport := pxy.NewRetryTransport(routeRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, routeNetError{}
			}))
			handler := pxy.NewProxyHandler(
				transport,
				func(*http.Request) {},
				nil,
				newErrorHandler(&testEffectiveConfig().Config),
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadGateway {
				t.Fatalf("response code = %d, want 502; body=%q", response.Code, response.Body.String())
			}
			if reporter.tcpCalls != 1 {
				t.Fatalf("TCP failure reports = %d, want one transport failure", reporter.tcpCalls)
			}
		})
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

	if err := newModifyResponse(&testEffectiveConfig().Config)(resp); err != nil {
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

func TestDefaultRequestBufferingMakesIncomingPostReplayable(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", &countingReadCloser{Reader: strings.NewReader("replay-body")})
	if request.GetBody != nil {
		t.Fatal("fixture already replayable")
	}
	cleanup, err := bufferRequestBodyIfNeeded(request)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	if request.GetBody == nil {
		t.Fatal("default buffering did not make POST replayable")
	}
	var attempts int
	retried := pxy.WithRetries(request, 1, func(*http.Request) bool { return true })
	response, err := pxy.NewRetryTransport(routeRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("dial failed")
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil || string(body) != "replay-body" {
			t.Fatalf("retry body=%q error=%v", body, readErr)
		}
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	})).RoundTrip(retried)
	if err != nil || response == nil || response.StatusCode != 204 || attempts != 2 {
		t.Fatalf("attempts=%d response=%v error=%v", attempts, response, err)
	}
}

func TestBufferedRequestAboveMemoryThresholdRemainsReplayable(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), int(proxy_control.DefaultRequestBufferingLimit)+1)
	request := proxy_control.WithRequestBuffering(
		httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload)),
		true,
	)
	cleanup, err := bufferRequestBodyIfNeeded(request)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("large request was rejected: %v", err)
	}
	if request.GetBody == nil {
		t.Fatal("large body is not replayable")
	}
	replay, err := request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replay.Close() }()
	got, err := io.ReadAll(replay)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("replay len=%d error=%v", len(got), err)
	}
}

func TestDefaultAndLargeBufferedPostRetryBeforeSend(t *testing.T) {
	for _, test := range []struct {
		name     string
		disabled bool
		size     int
	}{{"default", false, 12}, {"spooled", false, int(proxy_control.DefaultRequestBufferingLimit) + 1}, {"disabled", true, 12}} {
		t.Run(test.name, func(t *testing.T) {
			payload := strings.Repeat("x", test.size)
			var received string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				received = string(body)
				w.WriteHeader(204)
			}))
			defer upstream.Close()
			route := testRouteFromJSON(
				t,
				fmt.Sprintf(
					`{"id":"post-retry","uri":"/","upstream":{"nodes":{"127.0.0.1:1":1,%q:1}}}`,
					upstream.Listener.Addr().String(),
				),
			)
			handler := testPreparedProxyHandler(t, route, resource.Service{}, testEffectiveConfig())
			request := httptest.NewRequest(
				http.MethodPost,
				"http://gateway.test/",
				&countingReadCloser{Reader: strings.NewReader(payload)},
			)
			if test.disabled {
				request = proxy_control.WithRequestBuffering(request, false)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if test.disabled {
				if response.Code != 502 || received != "" {
					t.Fatalf("disabled status=%d received=%d", response.Code, len(received))
				}
				return
			}
			if response.Code != 204 || received != payload {
				t.Fatalf(
					"status=%d received=%d want=%d body=%s",
					response.Code,
					len(received),
					len(payload),
					response.Body.String(),
				)
			}
		})
	}
}

func TestRequestBodySpoolIsUnlinkedAndClosedWithOwner(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	payload := bytes.Repeat([]byte("x"), int(proxy_control.DefaultRequestBufferingLimit)+1)
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
	cleanup, err := bufferRequestBodyIfNeeded(request)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("spooled body has no cleanup owner")
	}
	defer cleanup()
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 {
		t.Fatalf("spool directory=%v error=%v", files, err)
	}
	replay, err := request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(replay)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("replay len=%d error=%v", len(got), err)
	}
	cleanup()
	replay, err = request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(replay); err == nil {
		t.Fatal("retired spool descriptor is still readable")
	}
}
