package proxy_mirror

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type mirrorRequest struct {
	Method string
	Path   string
	Query  string
	Host   string
	Header http.Header
	Body   string
}

type readSpyBody struct {
	io.Reader
	reads int
}

type blockingBody struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return 0, io.EOF
}

func (*blockingBody) Close() error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type closeRecordingTransport struct {
	started    chan struct{}
	release    <-chan struct{}
	canceled   chan struct{}
	closed     chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
	closeOnce  sync.Once
	closeCount atomic.Int32
}

func (t *closeRecordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.started != nil {
		t.startOnce.Do(func() { close(t.started) })
	}
	if t.release != nil {
		select {
		case <-t.release:
		case <-r.Context().Done():
			if t.canceled != nil {
				t.cancelOnce.Do(func() { close(t.canceled) })
			}
			<-t.release
		}
	}
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

func (t *closeRecordingTransport) CloseIdleConnections() {
	t.closeCount.Add(1)
	if t.closed != nil {
		t.closeOnce.Do(func() { close(t.closed) })
	}
}

func (b *readSpyBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (*readSpyBody) Close() error { return nil }

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func newMirrorServer(t *testing.T) (*httptest.Server, <-chan mirrorRequest) {
	t.Helper()

	seen := make(chan mirrorRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read mirror body: %v", err)
		}
		seen <- mirrorRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Host:   r.Host,
			Header: r.Header.Clone(),
			Body:   string(body),
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	return server, seen
}

func TestHandlerMirrorsRequestAndPreservesUpstreamBody(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{
		Host: mirror.URL,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/original?x=1", strings.NewReader("payload"))
	req.Header.Set("X-Test", "yes")
	req.Host = "original.example"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			t.Fatalf("run before-proxy hook: %v", err)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if got := string(body); got != "payload" {
			t.Fatalf("upstream body = %q, want payload", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}

	mirrored := waitForMirror(t, seen)
	if mirrored.Method != http.MethodPost {
		t.Fatalf("mirror method = %q, want POST", mirrored.Method)
	}
	if mirrored.Path != "/original" || mirrored.Query != "x=1" {
		t.Fatalf("mirror target = %s?%s, want /original?x=1", mirrored.Path, mirrored.Query)
	}
	if mirrored.Body != "payload" {
		t.Fatalf("mirror body = %q, want payload", mirrored.Body)
	}
	if got := mirrored.Header.Get("X-Test"); got != "yes" {
		t.Fatalf("mirror X-Test = %q, want yes", got)
	}
	if mirrored.Host != mustParseURL(t, mirror.URL).Host {
		t.Fatalf("mirror Host = %q, want mirror target host %q", mirrored.Host, mustParseURL(t, mirror.URL).Host)
	}
}

func TestMirrorStripsHopByHopAndSensitiveHeadersByDefault(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{Host: mirror.URL})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/original", nil)
	req.Header = http.Header{
		"Authorization":       {"Bearer secret"},
		"Proxy-Authorization": {"Basic secret"},
		"Cookie":              {"session=secret"},
		"Set-Cookie":          {"session=secret"},
		"Api-Key":             {"secret"},
		"apikey":              {"secret"},
		"X-API-KEY":           {"secret"},
		"X-Rbac-Token":        {"secret"},
		"X-Functions-Key":     {"secret"},
		"X-Goog-Api-Key":      {"secret"},
		"X-Amz-Signature":     {"secret"},
		"X-Trace":             {"keep"},
		"Connection":          {"keep-alive, X-Connection-Token"},
		"X-Connection-Token":  {"remove"},
		"Proxy-Connection":    {"remove"},
		"Keep-Alive":          {"remove"},
		"TE":                  {"remove"},
		"Trailer":             {"remove"},
		"Transfer-Encoding":   {"remove"},
		"Upgrade":             {"remove"},
		"Content-Length":      {"remove"},
		"Host":                {"remove"},
	}
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			t.Fatalf("run before-proxy hook: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	mirrored := waitForMirror(t, seen)
	for _, name := range []string{
		"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "Api-Key", "apikey", "X-API-KEY",
		"X-Rbac-Token", "X-Functions-Key", "X-Goog-Api-Key", "X-Amz-Signature",
		"Connection", "X-Connection-Token", "Proxy-Connection",
		"Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length", "Host",
	} {
		if got := mirrored.Header.Get(name); got != "" {
			t.Errorf("mirrored %s = %q, want stripped", name, got)
		}
	}
	if got := mirrored.Header.Get("X-Trace"); got != "keep" {
		t.Fatalf("mirrored X-Trace = %q, want keep", got)
	}
}

func TestMirrorKeepsSensitiveHeadersWhenExplicitlyEnabled(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{Host: mirror.URL, KeepSensitiveHeaders: true})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/original", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("X-API-KEY", "secret")
	req.Header.Set("X-Amz-Signature", "signature")
	req.Header.Set("Connection", "X-Connection-Token")
	req.Header.Set("X-Connection-Token", "remove")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			t.Fatalf("run before-proxy hook: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	mirrored := waitForMirror(t, seen)
	for _, test := range []struct {
		name string
		want string
	}{
		{name: "Authorization", want: "Bearer secret"},
		{name: "Cookie", want: "session=secret"},
		{name: "X-API-KEY", want: "secret"},
		{name: "X-Amz-Signature", want: "signature"},
	} {
		if got := mirrored.Header.Get(test.name); got != test.want {
			t.Errorf("mirrored %s = %q, want %q", test.name, got, test.want)
		}
	}
	if got := mirrored.Header.Get("X-Connection-Token"); got != "" {
		t.Errorf("mirrored X-Connection-Token = %q, want stripped", got)
	}
}

func TestBeforeProxyRejectsOversizedMirrorBodyBeforePrimaryUpstream(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com", MaxBodySize: 5})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/original", strings.NewReader("123456789"))
	var hookErr error
	var forwarded *http.Request
	p.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		forwarded = r
		hookErr = apisixctx.RunBeforeProxyHooks(r)
	})).ServeHTTP(httptest.NewRecorder(), req)

	if !base.IsBodyTooLarge(hookErr) {
		t.Fatalf("before-proxy error = %v, want body-too-large", hookErr)
	}
	restored, err := io.ReadAll(forwarded.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if got := string(restored); got != "123456" {
		t.Fatalf("retained body = %q, want limit+1 bytes", got)
	}
}

func TestHandlerMirrorsFinalizedRequestAfterLowerPriorityPlugins(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{Host: mirror.URL})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/original", strings.NewReader("before"))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/final"
		r.Body = io.NopCloser(strings.NewReader("after"))
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			t.Fatalf("run before-proxy hook: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	mirrored := waitForMirror(t, seen)
	if mirrored.Path != "/final" || mirrored.Body != "after" {
		t.Fatalf("mirrored finalized request = %s %q, want /final after", mirrored.Path, mirrored.Body)
	}
}

func TestHandlerRegistersBeforeProxyHookWithoutExecutingIt(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{Host: mirror.URL})
	body := &readSpyBody{Reader: strings.NewReader("payload")}
	req := httptest.NewRequest(http.MethodPost, "http://example.com/original", body)
	var forwarded *http.Request

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), req)

	if body.reads != 0 {
		t.Fatalf("request body reads during rewrite phase = %d, want 0", body.reads)
	}
	if forwarded == nil {
		t.Fatal("next handler did not receive the request carrying the hook")
	}

	if err := apisixctx.RunBeforeProxyHooks(forwarded); err != nil {
		t.Fatalf("run before-proxy hook: %v", err)
	}
	if body.reads == 0 {
		t.Fatal("registered before-proxy hook did not read the request body")
	}
	if mirrored := waitForMirror(t, seen); mirrored.Path != "/original" {
		t.Fatalf("mirrored path = %q, want /original", mirrored.Path)
	}
}

func TestProxyMirrorHookRegistrationCarriesOwnerAndPhase(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	var forwarded *http.Request
	p.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		forwarded = r
	})).ServeHTTP(httptest.NewRecorder(), request)
	if forwarded == nil {
		t.Fatal("next handler did not receive registered request")
	}
	calls := 0
	err := apisixctx.RunBeforeProxyHookRegistrations(
		forwarded,
		func(registration apisixctx.BeforeProxyHookRegistration) error {
			calls++
			if registration.Owner != "proxy-mirror" || registration.Phase != "before_proxy" {
				t.Fatalf("registration = %#v", registration)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("registration calls = %d, want 1", calls)
	}
}

func TestMirrorAdmissionBoundsInFlightAndStopCancels(t *testing.T) {
	started := make(chan struct{}, maxInFlightMirrors+1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	activeZero := make(chan struct{}, 1)
	var active atomic.Int32
	var maxActive atomic.Int32
	var totalStarted atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		totalStarted.Add(1)
		started <- struct{}{}
		<-release
		if active.Add(-1) == 0 {
			activeZero <- struct{}{}
		}
	}))
	releaseMirrors := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		releaseMirrors()
		mirror.Close()
	}()

	p := newTestPlugin(t, Config{Host: mirror.URL})
	for range maxInFlightMirrors {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/mirror", strings.NewReader("payload"))
		if err := p.mirrorFinalizedRequest(req); err != nil {
			t.Fatalf("mirrorFinalizedRequest() error = %v", err)
		}
	}
	for i := range maxInFlightMirrors {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for mirror %d/%d to start", i+1, maxInFlightMirrors)
		}
	}

	body := &readSpyBody{Reader: strings.NewReader("dropped")}
	dropped := httptest.NewRequest(http.MethodPost, "http://example.com/dropped", body)
	if err := p.mirrorFinalizedRequest(dropped); err != nil {
		t.Fatalf("saturated mirrorFinalizedRequest() error = %v", err)
	}
	if body.reads != 0 {
		t.Fatalf("saturated request body reads = %d, want 0", body.reads)
	}
	if got := totalStarted.Load(); got != maxInFlightMirrors {
		t.Fatalf("started mirrors = %d, want %d", got, maxInFlightMirrors)
	}
	if got := maxActive.Load(); got > maxInFlightMirrors {
		t.Fatalf("maximum active mirrors = %d, want <= %d", got, maxInFlightMirrors)
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Plugin.Stop() did not cancel and join blocked mirrors")
	}
	releaseMirrors()
	select {
	case <-activeZero:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked mirror handlers did not observe cancellation")
	}

	postStopBody := &readSpyBody{Reader: strings.NewReader("post-stop")}
	postStop := httptest.NewRequest(http.MethodPost, "http://example.com/post-stop", postStopBody)
	if err := p.mirrorFinalizedRequest(postStop); err != nil {
		t.Fatalf("post-stop mirrorFinalizedRequest() error = %v", err)
	}
	if postStopBody.reads != 0 {
		t.Fatalf("post-stop request body reads = %d, want 0", postStopBody.reads)
	}
	if got := totalStarted.Load(); got != maxInFlightMirrors {
		t.Fatalf("post-stop started mirrors = %d, want %d", got, maxInFlightMirrors)
	}
	if got := active.Load(); got != 0 {
		t.Fatalf("active mirrors after Stop() = %d, want 0", got)
	}
}

func TestConcurrentStopCallsWaitForInFlightMirrors(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})
	if err := p.mirrorFinalizedRequest(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/mirror",
		strings.NewReader("payload"),
	)); err != nil {
		t.Fatalf("mirrorFinalizedRequest() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mirror transport")
	}

	firstDone := make(chan struct{})
	go func() {
		p.Stop()
		close(firstDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mirrorMu.Lock()
		stopped := p.mirrorStopped
		p.mirrorMu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Stop() did not mark the plugin stopped")
		}
		time.Sleep(time.Millisecond)
	}

	secondDone := make(chan struct{})
	go func() {
		p.Stop()
		close(secondDone)
	}()
	select {
	case <-firstDone:
		t.Fatal("first Stop() returned before in-flight mirror completed")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-secondDone:
		t.Fatal("concurrent Stop() returned before in-flight mirror completed")
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first Stop() did not join in-flight mirror")
	}
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Stop() did not wait for first Stop()")
	}
}

func TestStopClosesIdleConnectionsAfterMirrorsDrain(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseMirrors := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseMirrors)

	clientTransport := &closeRecordingTransport{
		started:  make(chan struct{}),
		release:  release,
		canceled: make(chan struct{}),
		closed:   make(chan struct{}),
	}
	h2cTransport := &closeRecordingTransport{
		closed: make(chan struct{}),
	}
	p.client.Transport = clientTransport
	p.h2cClient.Transport = h2cTransport

	if err := p.mirrorFinalizedRequest(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/mirror",
		strings.NewReader("payload"),
	)); err != nil {
		t.Fatalf("mirrorFinalizedRequest() error = %v", err)
	}
	select {
	case <-clientTransport.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mirror transport")
	}

	stopDone := make(chan struct{}, 2)
	for range 2 {
		go func() {
			p.Stop()
			stopDone <- struct{}{}
		}()
	}

	select {
	case <-clientTransport.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not cancel the in-flight mirror")
	}
	select {
	case <-clientTransport.closed:
		t.Fatal("client idle connections closed before mirrors drained")
	case <-h2cTransport.closed:
		t.Fatal("h2c idle connections closed before mirrors drained")
	case <-stopDone:
		t.Fatal("Stop() returned before mirrors drained")
	default:
	}

	releaseMirrors()
	for range 2 {
		select {
		case <-stopDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Stop() did not finish after mirrors drained")
		}
	}

	if got := clientTransport.closeCount.Load(); got != 1 {
		t.Fatalf("client CloseIdleConnections() calls = %d, want 1", got)
	}
	if got := h2cTransport.closeCount.Load(); got != 1 {
		t.Fatalf("h2c CloseIdleConnections() calls = %d, want 1", got)
	}

	p.Stop()
	if got := clientTransport.closeCount.Load(); got != 1 {
		t.Fatalf("client CloseIdleConnections() calls after repeated Stop() = %d, want 1", got)
	}
	if got := h2cTransport.closeCount.Load(); got != 1 {
		t.Fatalf("h2c CloseIdleConnections() calls after repeated Stop() = %d, want 1", got)
	}
}

func TestStopDoesNotWaitForPrimaryRequestBodyRead(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	body := &blockingBody{entered: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "http://example.com/mirror", nil)
	req.Body = body
	hookDone := make(chan error, 1)
	go func() {
		hookDone <- p.mirrorFinalizedRequest(req)
	}()
	select {
	case <-body.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for primary request body read")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() waited for a primary request body read")
	}

	close(body.release)
	select {
	case err := <-hookDone:
		if err != nil {
			t.Fatalf("mirrorFinalizedRequest() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("primary request body read did not finish")
	}
}

func TestPostInitConfiguresHTTP2Transport(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "https://mirror.example.com"})

	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", p.client.Transport)
	}
	if len(transport.TLSNextProto) == 0 {
		t.Fatal("client transport has no configured HTTP/2 protocol")
	}
}

func TestHandlerMirrorsUnaryGRPCOverHTTP2(t *testing.T) {
	seen := make(chan struct{}, 1)
	mirror := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("mirror protocol = HTTP/%d, want HTTP/2", r.ProtoMajor)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Errorf("mirror content type = %q, want application/grpc", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read mirrored gRPC body: %v", err)
		}
		if got := string(body); got != "\x00\x00\x00\x00\x03abc" {
			t.Errorf("mirrored gRPC body = %q, want unary frame", got)
		}
		seen <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	mirror.EnableHTTP2 = true
	mirror.StartTLS()
	t.Cleanup(mirror.Close)

	p := newTestPlugin(t, Config{Host: mirror.URL})
	p.client = mirror.Client()

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/greeter.SayHello",
		strings.NewReader("\x00\x00\x00\x00\x03abc"),
	)
	req.Header.Set("Content-Type", "application/grpc")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			t.Fatalf("run before-proxy hook: %v", err)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read main gRPC body: %v", err)
		}
		if got := string(body); got != "\x00\x00\x00\x00\x03abc" {
			t.Fatalf("main gRPC body = %q, want unary frame", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
	select {
	case <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mirrored gRPC request")
	}
}

func TestHandlerReplacesMirrorPath(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{
		Host: mirror.URL,
		Path: "/shadow",
	})

	performRequest(p, "http://example.com/original?x=1")

	mirrored := waitForMirror(t, seen)
	if mirrored.Path != "/shadow" || mirrored.Query != "x=1" {
		t.Fatalf("mirror target = %s?%s, want /shadow?x=1", mirrored.Path, mirrored.Query)
	}
}

func TestHandlerPrefixesMirrorPath(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{
		Host:           mirror.URL,
		Path:           "/shadow",
		PathConcatMode: "prefix",
	})

	performRequest(p, "http://example.com/original?x=1")

	mirrored := waitForMirror(t, seen)
	if mirrored.Path != "/shadow/original" || mirrored.Query != "x=1" {
		t.Fatalf("mirror target = %s?%s, want /shadow/original?x=1", mirrored.Path, mirrored.Query)
	}
}

func TestSchemaRejectsHostWithPath(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{"host": "http://mirror.example.com/base"}, p.GetSchema())
	if err == nil {
		t.Fatal("schema accepted host with path, want rejection")
	}
}

func TestSchemaRejectsPathWithQueryDelimiter(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{
		"host": "http://mirror.example.com",
		"path": "/shadow?debug=true",
	}, p.GetSchema())
	if err == nil {
		t.Fatal("schema accepted path with query delimiter, want rejection")
	}
}

func TestSchemaAcceptsOfficialHTTPAndHTTPSHosts(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, host := range []string{
		"http://mirror.example.com",
		"https://mirror.example.com:9443",
		"http://127.0.0.1:9080",
		"http://[2001:db8::1]:9080",
		"grpc://mirror.example.com:9080",
		"grpcs://mirror.example.com:9443",
	} {
		t.Run(host, func(t *testing.T) {
			err := util.Validate(map[string]any{
				"host": host,
				"path": "/shadow",
			}, p.GetSchema())
			if err != nil {
				t.Fatalf("schema rejected %s: %v", host, err)
			}
		})
	}
}

func performRequest(p *Plugin, rawURL string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	return rr
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", rawURL, err)
	}
	return parsed
}

func waitForMirror(t *testing.T, seen <-chan mirrorRequest) mirrorRequest {
	t.Helper()

	select {
	case req := <-seen:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mirrored request")
		return mirrorRequest{}
	}
}
