package proxy_mirror

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
	"golang.org/x/net/dns/dnsmessage"
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

func withMirrorLifecycle(req *http.Request) (*http.Request, *apisixctx.RequestLifecycle) {
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	return apisixctx.WithRequestLifecycle(req, lifecycle), lifecycle
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

type testDNSServer struct {
	address   string
	questions chan dnsmessage.Question
	errors    chan error
}

func newTestDNSServer(t *testing.T, respond bool) *testDNSServer {
	t.Helper()

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen for test DNS: %v", err)
	}
	server := &testDNSServer{
		address:   conn.LocalAddr().String(),
		questions: make(chan dnsmessage.Question, 16),
		errors:    make(chan error, 1),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		for {
			n, peer, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			var query dnsmessage.Message
			if unpackErr := query.Unpack(buffer[:n]); unpackErr != nil {
				select {
				case server.errors <- fmt.Errorf("unpack DNS query: %w", unpackErr):
				default:
				}
				continue
			}
			if len(query.Questions) != 1 {
				select {
				case server.errors <- fmt.Errorf("DNS question count = %d, want 1", len(query.Questions)):
				default:
				}
				continue
			}
			question := query.Questions[0]
			select {
			case server.questions <- question:
			default:
			}
			if !respond {
				continue
			}

			response := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 query.ID,
					Response:           true,
					Authoritative:      true,
					RecursionAvailable: true,
				},
				Questions: query.Questions,
			}
			if question.Type == dnsmessage.TypeA {
				response.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  question.Name,
						Type:  dnsmessage.TypeA,
						Class: dnsmessage.ClassINET,
						TTL:   60,
					},
					Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
				}}
			}
			wire, packErr := response.Pack()
			if packErr != nil {
				select {
				case server.errors <- fmt.Errorf("pack DNS response: %w", packErr):
				default:
				}
				continue
			}
			if _, writeErr := conn.WriteToUDP(wire, peer); writeErr != nil {
				select {
				case server.errors <- fmt.Errorf("write DNS response: %w", writeErr):
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})

	return server
}

func (s *testDNSServer) waitForQuestion(t *testing.T) dnsmessage.Question {
	t.Helper()

	select {
	case question := <-s.questions:
		return question
	case err := <-s.errors:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DNS query")
	}
	return dnsmessage.Question{}
}

func TestHandlerMirrorsMaterializedUpstreamHostAndPreservesUpstreamBody(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{
		Host: mirror.URL,
	})

	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/original?x=1",
		strings.NewReader("payload"),
	))
	t.Cleanup(func() { lifecycle.FinalizeResult() })
	req.Header.Set("X-Test", "yes")
	req.Host = "gateway.example.test"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the route-side before-proxy contract: expose the effective
		// Host after pass_host handling, rather than the ingress authority.
		r.Host = "differential.example.test"
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
	if mirrored.Host != "differential.example.test" {
		t.Fatalf("mirror Host = %q, want materialized upstream Host", mirrored.Host)
	}
}

func TestHTTPSMirrorPreservesLogicalSNIAndMaterializedHost(t *testing.T) {
	dns := newTestDNSServer(t, true)
	sni := make(chan string, 1)
	seen := make(chan mirrorRequest, 1)
	mirror := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- mirrorRequest{Host: r.Host}
		w.WriteHeader(http.StatusNoContent)
	}))
	mirror.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case sni <- info.ServerName:
			default:
			}
			return nil, nil
		},
	}
	mirror.StartTLS()
	t.Cleanup(mirror.Close)
	parsed, err := url.Parse(mirror.URL)
	if err != nil {
		t.Fatalf("parse mirror URL: %v", err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("split mirror address: %v", err)
	}

	p := &Plugin{config: Config{Host: "https://example.com:" + port}}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{Config: config.Config{
		Apisix: config.Apisix{DnsResolver: []string{dns.address}, ResolverTimeout: 1},
	}}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", p.client.Transport)
	}
	mirrorTransport, ok := mirror.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("mirror client transport = %T, want *http.Transport", mirror.Client().Transport)
	}
	transport.TLSClientConfig.RootCAs = mirrorTransport.TLSClientConfig.RootCAs

	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodGet, "http://gateway.example.test/original", nil,
	))
	req.Host = "materialized-upstream.example.test"
	if err := p.mirrorFinalizedRequest(req); err != nil {
		t.Fatalf("mirrorFinalizedRequest() error = %v", err)
	}
	if result := lifecycle.FinalizeResult(); len(result.Failures) != 0 || result.FatalPanic != nil {
		t.Fatalf("finalization result = %#v, want success", result)
	}

	if question := dns.waitForQuestion(t); question.Name.String() != "example.com." {
		t.Fatalf("resolved host = %q, want logical mirror host example.com.", question.Name.String())
	}
	select {
	case got := <-sni:
		if got != "example.com" {
			t.Fatalf("TLS ClientHello ServerName = %q, want example.com", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS ClientHello")
	}
	if mirrored := waitForMirror(t, seen); mirrored.Host != "materialized-upstream.example.test" {
		t.Fatalf("mirrored Host = %q, want materialized upstream Host", mirrored.Host)
	}
}

func TestConfiguredResolverIsGenerationPinned(t *testing.T) {
	first := newTestDNSServer(t, true)
	second := newTestDNSServer(t, true)
	effective := &config.EffectiveConfig{Config: config.Config{
		Apisix: config.Apisix{
			DnsResolver:     []string{first.address, second.address},
			ResolverTimeout: 1,
		},
	}}
	p := &Plugin{config: Config{Host: "http://mirror.internal"}}
	p.SetDependencies(base.Dependencies{Config: effective})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	effective.Config.Apisix.DnsResolver[0] = "127.0.0.1:1"
	effective.Config.Apisix.DnsResolver[1] = "127.0.0.1:2"
	effective.Config.Apisix.ResolverTimeout = 9
	for i := range 2 {
		addresses, err := p.lookupNetIP(context.Background(), "ip4", "mirror.internal")
		if err != nil {
			t.Fatalf("LookupNetIP() call %d error = %v", i+1, err)
		}
		if len(addresses) != 1 || addresses[0] != netip.MustParseAddr("127.0.0.1") {
			t.Fatalf("LookupNetIP() call %d addresses = %v, want [127.0.0.1]", i+1, addresses)
		}
	}
	for index, server := range []*testDNSServer{first, second} {
		question := server.waitForQuestion(t)
		if question.Name.String() != "mirror.internal." || question.Type != dnsmessage.TypeA {
			t.Fatalf("DNS server %d question = %#v, want mirror.internal. A", index+1, question)
		}
	}
	if p.resolverTimeout != time.Second {
		t.Fatalf("resolver timeout = %s, want 1s pinned before config mutation", p.resolverTimeout)
	}

	for _, test := range []struct {
		server string
		want   string
	}{
		{server: "192.0.2.53", want: "192.0.2.53:53"},
		{server: "2001:db8::53", want: "[2001:db8::53]:53"},
		{server: "[2001:db8::53]:5353", want: "[2001:db8::53]:5353"},
	} {
		if got := mirrorDNSServerAddress(test.server); got != test.want {
			t.Fatalf("mirrorDNSServerAddress(%q) = %q, want %q", test.server, got, test.want)
		}
	}
}

func TestConfiguredResolverTimeoutBoundsLookup(t *testing.T) {
	dns := newTestDNSServer(t, false)
	p := &Plugin{config: Config{Host: "http://mirror.timeout.test"}}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{Config: config.Config{
		Apisix: config.Apisix{DnsResolver: []string{dns.address}, ResolverTimeout: 1},
	}}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	started := time.Now()
	_, err := p.dialMirrorContext(context.Background(), "tcp", "mirror.timeout.test:443")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("dialMirrorContext() error = nil, want resolver timeout")
	}
	if elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("resolver elapsed = %s, want configured 1s timeout", elapsed)
	}
	question := dns.waitForQuestion(t)
	if question.Name.String() != "mirror.timeout.test." {
		t.Fatalf("resolved host = %q, want mirror.timeout.test.", question.Name.String())
	}
}

func TestMirrorPreservesRequestHeadersExceptHopByHop(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{Host: mirror.URL})
	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(http.MethodGet, "http://example.com/original", nil))
	t.Cleanup(func() { lifecycle.FinalizeResult() })
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
	for name, want := range map[string]string{
		"Authorization": "Bearer secret", "Proxy-Authorization": "Basic secret",
		"Cookie": "session=secret", "Set-Cookie": "session=secret", "Api-Key": "secret",
		"apikey": "secret", "X-API-KEY": "secret", "X-Rbac-Token": "secret",
		"X-Functions-Key": "secret", "X-Goog-Api-Key": "secret", "X-Amz-Signature": "secret",
		"X-Trace": "keep",
	} {
		if got := mirrored.Header.Get(name); got != want {
			t.Errorf("mirrored %s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{
		"Connection", "X-Connection-Token", "Proxy-Connection",
		"Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length", "Host",
	} {
		if got := mirrored.Header.Get(name); got != "" {
			t.Errorf("mirrored %s = %q, want stripped", name, got)
		}
	}
}

func TestBeforeProxyRejectsOversizedMirrorBodyBeforePrimaryUpstream(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	p.maxBodySize = 5
	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/original",
		strings.NewReader("123456789"),
	))
	t.Cleanup(func() { lifecycle.FinalizeResult() })
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
	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/original",
		strings.NewReader("before"),
	))
	t.Cleanup(func() { lifecycle.FinalizeResult() })
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
	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(http.MethodPost, "http://example.com/original", body))
	t.Cleanup(func() { lifecycle.FinalizeResult() })
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

func TestMirrorAdmissionBoundsInFlightAndStopDoesNotCancel(t *testing.T) {
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
	lifecycles := make([]*apisixctx.RequestLifecycle, 0, maxInFlightMirrors+1)
	for range maxInFlightMirrors {
		req, lifecycle := withMirrorLifecycle(httptest.NewRequest(
			http.MethodPost,
			"http://example.com/mirror",
			strings.NewReader("payload"),
		))
		if err := p.mirrorFinalizedRequest(req); err != nil {
			t.Fatalf("mirrorFinalizedRequest() error = %v", err)
		}
		lifecycles = append(lifecycles, lifecycle)
	}
	for i := range maxInFlightMirrors {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for mirror %d/%d to start", i+1, maxInFlightMirrors)
		}
	}

	body := &readSpyBody{Reader: strings.NewReader("dropped")}
	dropped, droppedLifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/dropped",
		body,
	))
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
		t.Fatal("Plugin.Stop() waited for request-owned mirrors")
	}
	releaseMirrors()
	select {
	case <-activeZero:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked mirror handlers did not finish after release")
	}

	postStopBody := &readSpyBody{Reader: strings.NewReader("post-stop")}
	postStop, postStopLifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/post-stop",
		postStopBody,
	))
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
	droppedLifecycle.FinalizeResult()
	postStopLifecycle.FinalizeResult()
	for _, lifecycle := range lifecycles {
		lifecycle.FinalizeResult()
	}
}

func TestConcurrentStopCallsDoNotOwnInFlightMirrors(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
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
	releaseMirror := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseMirror)
	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/mirror",
		strings.NewReader("payload"),
	))
	if err := p.mirrorFinalizedRequest(req); err != nil {
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
	secondDone := make(chan struct{})
	go func() {
		p.Stop()
		close(secondDone)
	}()
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first Stop() waited for request-owned mirror")
	}
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Stop() waited for request-owned mirror")
	}

	finalized := make(chan apisixctx.FinalizationResult, 1)
	go func() { finalized <- lifecycle.FinalizeResult() }()
	select {
	case result := <-finalized:
		t.Fatalf("request lifecycle finalized before delivery release: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}
	releaseMirror()
	select {
	case result := <-finalized:
		if len(result.Failures) != 0 || result.FatalPanic != nil {
			t.Fatalf("finalization result = %#v, want success", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request lifecycle did not join in-flight mirror")
	}
}

func TestStopClosesIdleConnectionsWithoutOwningMirrors(t *testing.T) {
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

	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/mirror",
		strings.NewReader("payload"),
	))
	if err := p.mirrorFinalizedRequest(req); err != nil {
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
	case <-clientTransport.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("client idle connections were not closed by Stop()")
	}
	select {
	case <-h2cTransport.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("h2c idle connections were not closed by Stop()")
	}
	select {
	case <-clientTransport.canceled:
		t.Fatal("Stop() canceled the request-owned mirror")
	default:
	}

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return after closing idle connections")
	}
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Stop() did not return after closing idle connections")
	}

	finalized := make(chan apisixctx.FinalizationResult, 1)
	go func() { finalized <- lifecycle.FinalizeResult() }()
	select {
	case result := <-finalized:
		t.Fatalf("request lifecycle joined before delivery release: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}

	releaseMirrors()
	select {
	case result := <-finalized:
		if len(result.Failures) != 0 || result.FatalPanic != nil {
			t.Fatalf("finalization result = %#v, want success", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request lifecycle did not join mirror after release")
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
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/mirror", nil)
	req.Body = body
	req, lifecycle := withMirrorLifecycle(req)
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
	if result := lifecycle.FinalizeResult(); len(result.Failures) != 0 || result.FatalPanic != nil {
		t.Fatalf("finalization result = %#v, want success", result)
	}
}

func TestMirrorRequiresRequestLifecycleBeforeBodyRead(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	var started atomic.Int32
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		started.Add(1)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})
	body := &readSpyBody{Reader: strings.NewReader("payload")}
	req := httptest.NewRequest(http.MethodPost, "http://example.com/mirror", body)

	err := p.mirrorFinalizedRequest(req)
	if err == nil || err.Error() != "proxy-mirror request lifecycle is required" {
		t.Fatalf("mirrorFinalizedRequest() error = %v, want stable lifecycle error", err)
	}
	if body.reads != 0 {
		t.Fatalf("request body reads = %d, want 0", body.reads)
	}
	if started.Load() != 0 {
		t.Fatalf("mirror transport starts = %d, want 0", started.Load())
	}
}

func TestMirrorLifecycleWaitsForDelivery(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
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
	releaseMirrors := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseMirrors)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/mirror", strings.NewReader("payload"))
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	req = apisixctx.WithRequestLifecycle(req, lifecycle)

	if err := p.mirrorFinalizedRequest(req); err != nil {
		t.Fatalf("mirrorFinalizedRequest() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mirror transport")
	}
	if got := len(p.mirrorAdmission); got != 1 {
		t.Fatalf("admission tokens before finalization = %d, want 1", got)
	}

	finalized := make(chan apisixctx.FinalizationResult, 1)
	go func() { finalized <- lifecycle.FinalizeResult() }()
	select {
	case result := <-finalized:
		t.Fatalf("lifecycle finalized before mirror delivery: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}

	releaseMirrors()
	select {
	case result := <-finalized:
		if len(result.Failures) != 0 || result.FatalPanic != nil {
			t.Fatalf("finalization result = %#v, want success", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle finalization did not join mirror delivery")
	}
	if got := len(p.mirrorAdmission); got != 0 {
		t.Fatalf("admission tokens after finalization = %d, want 0", got)
	}
}

func TestMirrorTaskPanicIsBoundedByPluginFinalizer(t *testing.T) {
	if os.Getenv("APISIX_GO_PROXY_MIRROR_PANIC_HELPER") == "1" {
		testMirrorTaskPanicIsBoundedByPluginFinalizerHelper(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMirrorTaskPanicIsBoundedByPluginFinalizer$", "-test.v")
	cmd.Env = append(os.Environ(), "APISIX_GO_PROXY_MIRROR_PANIC_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy-mirror task panic escaped lifecycle owner: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("proxy-mirror-owner-recovered")) {
		t.Fatalf("missing lifecycle recovery marker in output: %s", out)
	}
}

func testMirrorTaskPanicIsBoundedByPluginFinalizerHelper(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	wantPanic := &struct{ marker string }{marker: "proxy-mirror"}
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		panic(wantPanic)
	})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/mirror", strings.NewReader("payload"))
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	req = apisixctx.WithRequestLifecycle(req, lifecycle)
	if err := p.mirrorFinalizedRequest(req); err != nil {
		t.Fatalf("mirrorFinalizedRequest() error = %v", err)
	}
	var later atomic.Int32
	if !lifecycle.AddFinalizer("later", func() error {
		later.Add(1)
		return nil
	}) {
		t.Fatal("failed to register later finalizer")
	}

	result := lifecycle.FinalizeResult()
	if later.Load() != 1 {
		t.Fatalf("later finalizer calls = %d, want 1", later.Load())
	}
	if result.FatalPanic != nil {
		t.Fatalf("proxy-mirror panic became fatal: %#v", result.FatalPanic)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("finalizer failures = %#v, want one proxy-mirror failure", result.Failures)
	}
	failure := result.Failures[0]
	if failure.Kind != apisixctx.FinalizerOwnerPlugin || failure.Owner != name || failure.PanicValue != wantPanic {
		t.Fatalf("proxy-mirror failure = %#v, want plugin owner and exact panic %#v", failure, wantPanic)
	}
	_, _ = fmt.Fprintln(os.Stdout, "proxy-mirror-owner-recovered")
}

func TestMirrorAdmissionRemainsBoundedPerRequest(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	releases := make(map[string]chan struct{}, maxInFlightMirrors+1)
	releaseOnce := make(map[string]*sync.Once, maxInFlightMirrors+1)
	started := make(chan string, maxInFlightMirrors+1)
	var transportMu sync.Mutex
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path := r.URL.Path
		started <- path
		transportMu.Lock()
		release := releases[path]
		transportMu.Unlock()
		<-release
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	lifecycles := make([]*apisixctx.RequestLifecycle, 0, maxInFlightMirrors+1)
	for i := range maxInFlightMirrors {
		path := fmt.Sprintf("/mirror/%d", i)
		transportMu.Lock()
		releases[path] = make(chan struct{})
		releaseOnce[path] = &sync.Once{}
		transportMu.Unlock()
		req := httptest.NewRequest(http.MethodPost, "http://example.com"+path, strings.NewReader("payload"))
		lifecycle := apisixctx.NewRequestLifecycle(time.Now())
		req = apisixctx.WithRequestLifecycle(req, lifecycle)
		if err := p.mirrorFinalizedRequest(req); err != nil {
			t.Fatalf("mirrorFinalizedRequest(%s) error = %v", path, err)
		}
		lifecycles = append(lifecycles, lifecycle)
	}
	transportMu.Lock()
	releases["/mirror/16"] = make(chan struct{})
	releaseOnce["/mirror/16"] = &sync.Once{}
	transportMu.Unlock()
	closeRelease := func(path string) {
		transportMu.Lock()
		release := releases[path]
		once := releaseOnce[path]
		transportMu.Unlock()
		if once != nil {
			once.Do(func() { close(release) })
		}
	}
	t.Cleanup(func() {
		transportMu.Lock()
		paths := make([]string, 0, len(releases))
		for path := range releases {
			paths = append(paths, path)
		}
		transportMu.Unlock()
		for _, path := range paths {
			closeRelease(path)
		}
	})
	for i := range maxInFlightMirrors {
		select {
		case got := <-started:
			if got == "" {
				t.Fatal("mirror transport reported an empty path")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for mirror %d/%d to start", i+1, maxInFlightMirrors)
		}
	}

	droppedBody := &readSpyBody{Reader: strings.NewReader("dropped")}
	dropped := httptest.NewRequest(http.MethodPost, "http://example.com/mirror/16", droppedBody)
	droppedLifecycle := apisixctx.NewRequestLifecycle(time.Now())
	dropped = apisixctx.WithRequestLifecycle(dropped, droppedLifecycle)
	if err := p.mirrorFinalizedRequest(dropped); err != nil {
		t.Fatalf("saturated mirrorFinalizedRequest() error = %v", err)
	}
	if droppedBody.reads != 0 {
		t.Fatalf("saturated request body reads = %d, want 0", droppedBody.reads)
	}

	firstFinalized := make(chan apisixctx.FinalizationResult, 1)
	go func() { firstFinalized <- lifecycles[0].FinalizeResult() }()
	closeRelease("/mirror/0")
	select {
	case result := <-firstFinalized:
		if len(result.Failures) != 0 || result.FatalPanic != nil {
			t.Fatalf("first finalization result = %#v, want success", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request lifecycle did not release its mirror")
	}

	nextBody := &readSpyBody{Reader: strings.NewReader("next")}
	next := httptest.NewRequest(http.MethodPost, "http://example.com/mirror/16", nextBody)
	nextLifecycle := apisixctx.NewRequestLifecycle(time.Now())
	next = apisixctx.WithRequestLifecycle(next, nextLifecycle)
	if err := p.mirrorFinalizedRequest(next); err != nil {
		t.Fatalf("post-completion mirrorFinalizedRequest() error = %v", err)
	}
	if nextBody.reads == 0 {
		t.Fatal("post-completion request body was not read after admission")
	}
	lifecycles = append(lifecycles, nextLifecycle)

	for path := range releases {
		if path == "/mirror/0" {
			continue
		}
		closeRelease(path)
	}
	for _, lifecycle := range lifecycles[1:] {
		lifecycle.FinalizeResult()
	}
}

func TestStopDoesNotOwnRequestTasks(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.com"})
	release := make(chan struct{})
	entered := make(chan struct{})
	canceled := make(chan struct{})
	var releaseOnce sync.Once
	transport := &closeRecordingTransport{
		started:  entered,
		release:  release,
		canceled: canceled,
	}
	p.client.Transport = transport
	releaseMirror := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseMirror)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/mirror", strings.NewReader("payload"))
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	req = apisixctx.WithRequestLifecycle(req, lifecycle)
	if err := p.mirrorFinalizedRequest(req); err != nil {
		t.Fatalf("mirrorFinalizedRequest() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mirror transport")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Plugin.Stop() waited for request-owned mirror task")
	}
	select {
	case <-canceled:
		t.Fatal("Plugin.Stop() canceled request-owned mirror task")
	default:
	}
	if got := transport.closeCount.Load(); got != 1 {
		t.Fatalf("client CloseIdleConnections() calls = %d, want 1", got)
	}

	finalized := make(chan apisixctx.FinalizationResult, 1)
	go func() { finalized <- lifecycle.FinalizeResult() }()
	select {
	case result := <-finalized:
		t.Fatalf("request lifecycle joined before delivery release: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}
	releaseMirror()
	select {
	case result := <-finalized:
		if len(result.Failures) != 0 || result.FatalPanic != nil {
			t.Fatalf("finalization result = %#v, want success", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request lifecycle did not join mirror task")
	}
	p.Stop()
	if got := transport.closeCount.Load(); got != 1 {
		t.Fatalf("client CloseIdleConnections() calls after repeated Stop() = %d, want 1", got)
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

	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/greeter.SayHello",
		strings.NewReader("\x00\x00\x00\x00\x03abc"),
	))
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
	if result := lifecycle.FinalizeResult(); len(result.Failures) != 0 || result.FatalPanic != nil {
		t.Fatalf("finalization result = %#v, want success", result)
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
	req, lifecycle := withMirrorLifecycle(httptest.NewRequest(http.MethodGet, rawURL, nil))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	lifecycle.FinalizeResult()
	return rr
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
