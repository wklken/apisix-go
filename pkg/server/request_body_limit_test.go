package server

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
)

const requestBodyLimitTestMessage = `{"message":"request body too large"}`

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func assertRequestBodyLimitResponse(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
	if got, want := response.Header().Get("Content-Type"), "application/json; charset=UTF-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if got := response.Body.String(); got != requestBodyLimitTestMessage {
		t.Fatalf("body = %q, want %q", got, requestBodyLimitTestMessage)
	}
}

func newLimitedRequest(t *testing.T, body io.ReadCloser, contentLength int64) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test/upload", body)
	request.ContentLength = contentLength
	return request
}

func TestRequestBodyLimitRejectsKnownOversizedContentLength(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader("abcd")}
	nextCalled := false
	handler := limitRequestBody(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}), 3)
	response := httptest.NewRecorder()
	response.Header().Set("X-Stale", "remove-me")

	handler.ServeHTTP(response, newLimitedRequest(t, body, 4))

	assertRequestBodyLimitResponse(t, response)
	if nextCalled {
		t.Fatal("downstream handler was called for a known oversized body")
	}
	if body.closed != true {
		t.Fatal("oversized request body was not closed")
	}
	if response.Header().Get("X-Stale") != "" {
		t.Fatal("stale response headers were not cleared")
	}
}

func TestRequestBodyLimitAllowsBoundaryAndRejectsFirstByteOverLimit(t *testing.T) {
	for _, test := range []struct {
		name          string
		body          string
		contentLength int64
		wantStatus    int
		wantBody      string
	}{
		{name: "known exact boundary", body: "abc", contentLength: 3, wantStatus: http.StatusNoContent},
		{name: "unknown exact boundary", body: "abc", contentLength: -1, wantStatus: http.StatusNoContent},
		{name: "unknown first byte over", body: "abcd", contentLength: -1, wantStatus: http.StatusRequestEntityTooLarge, wantBody: requestBodyLimitTestMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.ReadAll(r.Body); err != nil {
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}), 3)
			response := httptest.NewRecorder()

			handler.ServeHTTP(
				response,
				newLimitedRequest(t, io.NopCloser(strings.NewReader(test.body)), test.contentLength),
			)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantBody != "" {
				assertRequestBodyLimitResponse(t, response)
			} else if response.Body.Len() != 0 {
				t.Fatalf("body = %q, want empty", response.Body.String())
			}
		})
	}
}

func TestRequestBodyLimitStreamsBeforeProducerCompletes(t *testing.T) {
	reader, writer := io.Pipe()
	started := make(chan struct{})
	handler := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		var one [1]byte
		if _, err := io.ReadFull(r.Body, one[:]); err != nil {
			t.Errorf("ReadFull() error = %v", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), 3)
	response := httptest.NewRecorder()
	request := newLimitedRequest(t, reader, -1)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("streaming handler did not finish")
		}
	})

	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("downstream handler did not start before producer completed")
	}
	if _, err := writer.Write([]byte("a")); err != nil {
		t.Fatalf("pipe write = %v", err)
	}
	_ = writer.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("streaming handler did not finish after producer supplied one byte")
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
}

func TestRequestBodyLimitSuppressesDownstreamResponseAfterOverflow(t *testing.T) {
	handler := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Stale", "remove-me")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream failure"))
	}), 3)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, newLimitedRequest(t, io.NopCloser(strings.NewReader("abcd")), -1))

	assertRequestBodyLimitResponse(t, response)
	if response.Header().Get("X-Stale") != "" {
		t.Fatal("downstream response headers survived overflow")
	}
}

func TestRequestBodyLimitPreservesCommittedResponseAfterOverflow(t *testing.T) {
	handler := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
		_, _ = io.ReadAll(r.Body)
	}), 3)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, newLimitedRequest(t, io.NopCloser(strings.NewReader("abcd")), -1))

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	if got, want := response.Body.String(), "accepted"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

type allOptionalResponseWriter struct {
	header        http.Header
	body          bytes.Buffer
	closeNotify   chan bool
	flushErr      error
	readDeadline  time.Time
	writeDeadline time.Time
}

func newAllOptionalResponseWriter() *allOptionalResponseWriter {
	return &allOptionalResponseWriter{header: make(http.Header), closeNotify: make(chan bool)}
}

func (w *allOptionalResponseWriter) Header() http.Header            { return w.header }
func (*allOptionalResponseWriter) WriteHeader(int)                  {}
func (w *allOptionalResponseWriter) Write(body []byte) (int, error) { return w.body.Write(body) }
func (*allOptionalResponseWriter) Flush()                           {}
func (w *allOptionalResponseWriter) FlushError() error              { return w.flushErr }
func (w *allOptionalResponseWriter) CloseNotify() <-chan bool       { return w.closeNotify }

func (*allOptionalResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("hijack unavailable in test")
}

func (w *allOptionalResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(&w.body, reader)
}

func (w *allOptionalResponseWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline
	return nil
}

func (w *allOptionalResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}
func (*allOptionalResponseWriter) EnableFullDuplex() error              { return nil }
func (*allOptionalResponseWriter) Push(string, *http.PushOptions) error { return nil }
func (w *allOptionalResponseWriter) WriteString(value string) (int, error) {
	return w.body.WriteString(value)
}

func TestRequestBodyLimitPreservesExactResponseWriterOptionalInterfaces(t *testing.T) {
	for _, test := range []struct {
		name string
		new  func() http.ResponseWriter
	}{
		{name: "minimal", new: func() http.ResponseWriter { return httptest.NewRecorder() }},
		{name: "all optional", new: func() http.ResponseWriter { return newAllOptionalResponseWriter() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wrapped http.ResponseWriter
			handler := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				wrapped = w
			}), 1)
			original := test.new()
			handler.ServeHTTP(original, newLimitedRequest(t, io.NopCloser(strings.NewReader("")), 0))

			assertOptionalResponseWriterInterfaces(t, original, wrapped)
		})
	}
}

func assertOptionalResponseWriterInterfaces(t *testing.T, original, wrapped http.ResponseWriter) {
	t.Helper()
	checks := []struct {
		name string
		has  func(http.ResponseWriter) bool
	}{
		{name: "Flusher", has: func(w http.ResponseWriter) bool { _, ok := w.(http.Flusher); return ok }},
		{
			name: "FlushError",
			has:  func(w http.ResponseWriter) bool { _, ok := w.(interface{ FlushError() error }); return ok },
		},
		{
			name: "CloseNotifier",
			has:  func(w http.ResponseWriter) bool { _, ok := w.(interface{ CloseNotify() <-chan bool }); return ok },
		},
		{name: "Hijacker", has: func(w http.ResponseWriter) bool { _, ok := w.(http.Hijacker); return ok }},
		{name: "ReaderFrom", has: func(w http.ResponseWriter) bool { _, ok := w.(io.ReaderFrom); return ok }},
		{name: "deadliner", has: func(w http.ResponseWriter) bool {
			_, ok := w.(interface {
				SetReadDeadline(time.Time) error
				SetWriteDeadline(time.Time) error
			})
			return ok
		}},
		{
			name: "fullDuplex",
			has:  func(w http.ResponseWriter) bool { _, ok := w.(interface{ EnableFullDuplex() error }); return ok },
		},
		{name: "Pusher", has: func(w http.ResponseWriter) bool { _, ok := w.(http.Pusher); return ok }},
		{name: "StringWriter", has: func(w http.ResponseWriter) bool { _, ok := w.(io.StringWriter); return ok }},
	}
	for _, check := range checks {
		if check.has(original) != check.has(wrapped) {
			t.Errorf("%s parity: original=%v wrapped=%v", check.name, check.has(original), check.has(wrapped))
		}
	}
}

type unknownLengthReader struct {
	reader *strings.Reader
}

func (r *unknownLengthReader) Read(p []byte) (int, error) { return r.reader.Read(p) }

func TestRequestBodyLimitWithReverseProxyReturnsCanonicalOverflow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	gateway := httptest.NewServer(limitRequestBody(proxy, 3))
	t.Cleanup(gateway.Close)

	request, err := http.NewRequest(
		http.MethodPost,
		gateway.URL,
		&unknownLengthReader{reader: strings.NewReader("abcd")},
	)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(response.Body) error = %v", err)
	}

	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.StatusCode)
	}
	if got, want := string(body), requestBodyLimitTestMessage; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestConfiguredHTTPHandlerClosesHTTP1ConnectionAfterLongTailOverflow(t *testing.T) {
	cfg := &config.Config{NginxConfig: config.NginxConfig{
		HTTP: config.NginxHTTP{ClientMaxBodySize: 3},
	}}
	handler := newConfiguredHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}), cfg)
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = false
	server.Start()
	t.Cleanup(server.Close)

	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/upload",
		&unknownLengthReader{reader: strings.NewReader(strings.Repeat("a", 300<<10))},
	)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("server request error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, err = io.Copy(io.Discard, response.Body)
	if err != nil {
		t.Fatalf("drain response body error = %v", err)
	}

	assertRequestBodyLimitHTTPResponse(t, response)
	if !response.Close {
		t.Fatal("HTTP/1.1 connection remains reusable after long-tail body overflow")
	}
}

func TestConfiguredHTTPHandlerReturnsCanonicalOverflowBeforePanic(t *testing.T) {
	cfg := &config.Config{NginxConfig: config.NginxConfig{
		HTTP: config.NginxHTTP{ClientMaxBodySize: 3},
	}}
	routes := newRouteHandler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		panic("after request body overflow")
	}), nil)
	server := httptest.NewServer(newConfiguredHTTPHandler(routes, cfg))
	t.Cleanup(server.Close)

	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/upload",
		&unknownLengthReader{reader: strings.NewReader("abcd")},
	)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("server request error = %v, want canonical 413 response", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(response.Body) error = %v", err)
	}

	assertRequestBodyLimitHTTPResponse(t, response)
	if got := string(body); got != requestBodyLimitTestMessage {
		t.Fatalf("body = %q, want %q", got, requestBodyLimitTestMessage)
	}
}

func assertRequestBodyLimitHTTPResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.StatusCode)
	}
	if got, want := response.Header.Get("Content-Type"), "application/json; charset=UTF-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
}

func TestRequestBodyLimitReadFromDoesNotBlockOverflowState(t *testing.T) {
	state := &requestBodyLimitState{}
	underlyingStarted := make(chan struct{})
	releaseUnderlying := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUnderlying) }) }

	readFrom := state.readFrom(func(io.Reader) (int64, error) {
		close(underlyingStarted)
		<-releaseUnderlying
		return 0, nil
	})
	readFromDone := make(chan struct{})
	go func() {
		_, _ = readFrom(strings.NewReader("response"))
		close(readFromDone)
	}()
	t.Cleanup(func() {
		release()
		select {
		case <-readFromDone:
		case <-time.After(time.Second):
			t.Error("blocked ReadFrom did not finish during cleanup")
		}
	})

	select {
	case <-underlyingStarted:
	case <-time.After(time.Second):
		t.Fatal("underlying ReadFrom did not start")
	}
	rejected := make(chan struct{})
	go func() {
		state.reject()
		close(rejected)
	}()

	select {
	case <-rejected:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("overflow rejection blocked behind underlying ReadFrom")
	}
	release()
	select {
	case <-readFromDone:
	case <-time.After(time.Second):
		t.Fatal("ReadFrom did not finish after its blocker was released")
	}
}
