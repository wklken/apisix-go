package function_upstream

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func TestHandlerHonorsDisabledSSLVerify(t *testing.T) {
	function := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("function ok"))
	}))
	defer function.Close()

	sslVerify := false
	p := newTestPlugin(t, Config{FunctionURI: function.URL, SSLVerify: &sslVerify})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/serverless", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("function upstream should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d, body=%q", rr.Code, http.StatusCreated, rr.Body.String())
	}
	if got := rr.Body.String(); got != "function ok" {
		t.Fatalf("response body = %q, want function ok", got)
	}
}

func TestRunRequestPhasePublishesUpstreamSourceBeforeFunctionResponse(t *testing.T) {
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("function ok"))
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/serverless", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	response := httptest.NewRecorder()

	result := p.RunRequestPhase(response, request)
	if result.Decision != 1 || result.Source != apisixctx.ResponseSourceUpstream {
		t.Fatalf("result = %+v, want upstream stop", result)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceUpstream {
		t.Fatalf("source = %q, want upstream", lifecycle.ResponseSource())
	}
	if response.Code != http.StatusCreated || response.Body.String() != "function ok" {
		t.Fatalf("response = %d/%q, want 201/function ok", response.Code, response.Body.String())
	}
}

func TestHandlerRejectsSelfSignedFunctionWhenSSLVerifyDefaultsTrue(t *testing.T) {
	function := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/serverless", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("function upstream should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestPostInitAppliesKeepaliveTransportOptions(t *testing.T) {
	sslVerify := false
	keepalive := false
	p := newTestPlugin(t, Config{
		FunctionURI:      "https://function.example",
		SSLVerify:        &sslVerify,
		Keepalive:        &keepalive,
		KeepaliveTimeout: 1500,
		KeepalivePool:    7,
	})

	transport := p.transport()
	if !transport.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = false, want true")
	}
	if transport.IdleConnTimeout != 1500*time.Millisecond {
		t.Fatalf("IdleConnTimeout = %s, want 1500ms", transport.IdleConnTimeout)
	}
	if transport.MaxIdleConnsPerHost != 7 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 7", transport.MaxIdleConnsPerHost)
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLSClientConfig.InsecureSkipVerify should be true when ssl_verify=false")
	}
}

func TestPostInitSharesProgressTimeoutClientAndReleasesFinalReference(t *testing.T) {
	config := Config{FunctionURI: "http://function.example", Timeout: 743}
	first := newNamedTestPlugin(t, "aws-lambda", config)
	second := newNamedTestPlugin(t, "azure-functions", config)
	if first.Client != second.Client {
		t.Fatal("identical function transport configs did not share a client")
	}
	if first.Client.Timeout != 0 {
		t.Fatalf("http.Client.Timeout = %s, want 0", first.Client.Timeout)
	}

	first.Stop()
	third := newNamedTestPlugin(t, "openfunction", config)
	if third.Client != second.Client {
		t.Fatal("releasing one of two references closed the shared client")
	}
	second.Stop()
	third.Stop()

	recreated := newNamedTestPlugin(t, "aws-lambda", config)
	if recreated.Client == first.Client {
		t.Fatal("client was not recreated after the final release")
	}
	recreated.Stop()

	different := newNamedTestPlugin(t, "aws-lambda", Config{
		FunctionURI: "http://function.example", Timeout: 744,
	})
	if different.Client == recreated.Client {
		t.Fatal("different progress timeout reused the same client")
	}
	different.Stop()
}

func TestProgressTimeoutAllowsLongResponseWithContinuousChunks(t *testing.T) {
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for index, chunk := range []string{"a", "b", "c", "d"} {
			_, _ = io.WriteString(w, chunk)
			w.(http.Flusher).Flush()
			if index < 3 {
				time.Sleep(75 * time.Millisecond)
			}
		}
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL, Timeout: 200})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called")
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/function", nil))

	if response.Code != http.StatusOK || response.Body.String() != "abcd" {
		t.Fatalf("streamed response = %d/%q, want 200/abcd", response.Code, response.Body.String())
	}
}

func TestProgressTimeoutPreservesBodyCarryingRequests(t *testing.T) {
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read function request body: %v", err)
			return
		}
		_, _ = w.Write(body)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL, Timeout: 200})
	request := httptest.NewRequest(http.MethodPost, "/function", strings.NewReader("request body"))
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "request body" {
		t.Fatalf("function response = %d/%q, want 200/request body", response.Code, response.Body.String())
	}
}

func TestBuildRequestPropagatesMaxBytesError(t *testing.T) {
	p := newTestPlugin(t, Config{FunctionURI: "http://function.example"})
	body := &closeTrackingBody{Reader: strings.NewReader("oversized")}
	request := httptest.NewRequest(http.MethodPost, "/function", body)
	request.Body = http.MaxBytesReader(httptest.NewRecorder(), request.Body, 4)

	_, err := p.buildRequest(request)
	var maxBytesErr *http.MaxBytesError
	if !errors.As(err, &maxBytesErr) {
		t.Fatalf("buildRequest() error = %v, want *http.MaxBytesError", err)
	}
	if !body.closed {
		t.Fatal("buildRequest() did not close the bounded request body after the failed read")
	}
}

func TestProgressTimeoutRecordsIdleGapAfterCommit(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL, Timeout: 75})
	response := httptest.NewRecorder()
	recovered, capture := runCapturedRequestPhase(
		p,
		response,
		httptest.NewRequest(http.MethodGet, "/function", nil),
	)
	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
	}
	if response.Code != http.StatusOK || response.Body.String() != "first" {
		t.Fatalf("partial response = %d/%q, want 200/first", response.Code, response.Body.String())
	}
	if got := capture.Outcome().FailureReason; got != apisixctx.ResponseFailureUpstreamIdleTimeout {
		t.Fatalf("failure reason = %q, want upstream_idle_timeout", got)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("idle timeout did not cancel the upstream request")
	}
}

func TestProgressTimeoutRecordsResponseHeaderTimeoutBeforeCommit(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	function := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL, Timeout: 75})
	response := httptest.NewRecorder()
	recovered, capture := runCapturedRequestPhase(
		p,
		response,
		httptest.NewRequest(http.MethodGet, "/function", nil),
	)
	if recovered != nil {
		t.Fatalf("panic = %v, want nil before response commit", recovered)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if got := capture.Outcome().FailureReason; got != apisixctx.ResponseFailureUpstreamHeaderTimeout {
		t.Fatalf("failure reason = %q, want upstream_header_timeout", got)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("response header timeout did not cancel the upstream request")
	}
}

func TestProgressTimeoutPropagatesClientCancellation(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL, Timeout: 500})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/function", nil).WithContext(ctx)
	response := &notifyingResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		committed:        make(chan struct{}),
	}
	type result struct {
		panicValue any
		capture    *base.ResponseCapture
	}
	done := make(chan result, 1)
	go func() {
		recovered, capture := runCapturedRequestPhase(p, response, request)
		done <- result{panicValue: recovered, capture: capture}
	}()
	<-response.committed
	cancel()
	got := <-done
	if got.panicValue != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", got.panicValue)
	}
	if reason := got.capture.Outcome().FailureReason; reason != apisixctx.ResponseFailureClientCanceled {
		t.Fatalf("failure reason = %q, want client_canceled", reason)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not close the upstream request")
	}
}

func TestResponseCopyRecordsUpstreamHalfClose(t *testing.T) {
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = io.WriteString(w, "short")
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL})
	response := httptest.NewRecorder()
	recovered, capture := runCapturedRequestPhase(
		p,
		response,
		httptest.NewRequest(http.MethodGet, "/function", nil),
	)
	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
	}
	if reason := capture.Outcome().FailureReason; reason != apisixctx.ResponseFailureUpstreamCopyError {
		t.Fatalf("failure reason = %q, want upstream_copy_error", reason)
	}
}

func TestResponseCopyRecordsDownstreamWriteFailure(t *testing.T) {
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "response")
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL})
	recovered, capture := runCapturedRequestPhase(
		p,
		&failingResponseWriter{header: make(http.Header)},
		httptest.NewRequest(http.MethodGet, "/function", nil),
	)
	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
	}
	if reason := capture.Outcome().FailureReason; reason != apisixctx.ResponseFailureClientWriteError {
		t.Fatalf("failure reason = %q, want client_write_error", reason)
	}
}

func TestBuildRequestAppendsMatchedExtPath(t *testing.T) {
	p := newTestPlugin(t, Config{FunctionURI: "https://function.example/api/root"})
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("ext", "users/42")
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/functions/users/42?active=1", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))

	upstream, err := p.buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest() error = %v", err)
	}
	if upstream.URL.Path != "/api/root/users/42" {
		t.Fatalf("upstream path = %q, want /api/root/users/42", upstream.URL.Path)
	}
	if upstream.URL.RawQuery != "active=1" {
		t.Fatalf("upstream query = %q, want active=1", upstream.URL.RawQuery)
	}
}

func TestBuildRequestUsesChiWildcardAndNormalizesRepeatedSlashes(t *testing.T) {
	p := newTestPlugin(t, Config{FunctionURI: "https://function.example/api"})
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("*", "//http////trigger")
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/azure///http////trigger", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))

	upstream, err := p.buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest() error = %v", err)
	}
	if upstream.URL.Path != "/api/http/trigger" {
		t.Fatalf("upstream path = %q, want /api/http/trigger", upstream.URL.Path)
	}
}

func TestWriteResponseDropsConnectionHeadersForHTTP2(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Connection":        []string{"keep-alive"},
			"Keep-Alive":        []string{"timeout=5"},
			"Proxy-Connection":  []string{"keep-alive"},
			"Upgrade":           []string{"websocket"},
			"Transfer-Encoding": []string{"chunked"},
			"X-Result":          []string{"ok"},
		},
		Body: io.NopCloser(strings.NewReader("response")),
	}
	recorder := httptest.NewRecorder()
	if _, err := writeResponse(recorder, response, true, context.Background()); err != nil {
		t.Fatalf("writeResponse() error = %v", err)
	}

	for _, field := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Upgrade", "Transfer-Encoding"} {
		if got := recorder.Header().Get(field); got != "" {
			t.Fatalf("%s = %q, want removed", field, got)
		}
	}
	if got := recorder.Header().Get("X-Result"); got != "ok" {
		t.Fatalf("X-Result = %q, want ok", got)
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	return newNamedTestPlugin(t, "function-upstream", cfg)
}

func newNamedTestPlugin(t *testing.T, name string, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{Config: cfg}
	p.Name = name
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func runCapturedRequestPhase(
	p *Plugin,
	destination http.ResponseWriter,
	request *http.Request,
) (recovered any, capture *base.ResponseCapture) {
	wrapped, capture := base.CaptureResponseOutcomeController(destination)
	request = base.WithResponseCapture(request, capture)
	defer func() { recovered = recover() }()
	p.RunRequestPhase(wrapped, request)
	return nil, capture
}

type failingResponseWriter struct {
	header http.Header
}

type notifyingResponseWriter struct {
	*httptest.ResponseRecorder
	committed chan struct{}
	once      sync.Once
}

func (w *notifyingResponseWriter) WriteHeader(status int) {
	w.ResponseRecorder.WriteHeader(status)
	w.once.Do(func() { close(w.committed) })
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (*failingResponseWriter) WriteHeader(int)       {}
func (*failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client write failed")
}

func newQuietTLSServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(testLogWriter{}, "", 0)
	server.StartTLS()
	return server
}

type testLogWriter struct{}

func (testLogWriter) Write(p []byte) (int, error) {
	return len(p), nil
}
