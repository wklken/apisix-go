package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

// Deterministic proxy fault suite exercised through the public route handler:
// timeout policy, replay-safe retries, cluster admission, active health
// recovery, and error mapping.

type upstreamFaultMode string

const (
	faultNormal        upstreamFaultMode = "normal"
	faultHeaderTimeout upstreamFaultMode = "header-timeout"
	faultBodyStall     upstreamFaultMode = "body-stall"
	faultReset         upstreamFaultMode = "reset"
)

// faultUpstream is a switchable upstream fixture. The mode changes atomically
// and every request increments an attempt counter so tests assert exact retry
// counts through the public path.
type faultUpstream struct {
	mode     atomic.Value // upstreamFaultMode
	attempts atomic.Int32
}

func newFaultUpstream() *faultUpstream {
	fixture := &faultUpstream{}
	fixture.mode.Store(upstreamFaultMode(faultNormal))
	return fixture
}

func (fault *faultUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fault.attempts.Add(1)
	switch fault.mode.Load().(upstreamFaultMode) {
	case faultReset:
		// Reset the TCP connection after accepting the request so RoundTrip
		// observes a transport error instead of a response.
		if hijacker, ok := w.(http.Hijacker); ok {
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	case faultHeaderTimeout:
		// Accept the connection and stall response headers until the proxy
		// transport closes it on its response-header timeout. Hijack is
		// required so the handler goroutine unblocks when the connection is
		// closed instead of waiting on a request context that is never
		// cancelled.
		if hijacker, ok := w.(http.Hijacker); ok {
			connection, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			_, _ = connection.Read(make([]byte, 1))
			_ = connection.Close()
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	case faultBodyStall:
		// Commit status 200 and stall the body. The proxy progress read
		// timeout cancels the request context, which unblocks this handler.
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	default:
		_, _ = w.Write([]byte("ok-body"))
	}
}

// buildFaultHandler wires a single-node upstream through the public reverse
// handler. readTimeout seconds bounds both the response-header wait and body
// inactivity; retries applies to transport failures.
func buildFaultHandler(t *testing.T, targetURL string, readTimeout, retries int) http.Handler {
	t.Helper()
	upstream := resource.Upstream{
		Scheme:  "http",
		Nodes:   []resource.Node{upstreamNode(t, targetURL)},
		Retries: retries,
		Timeout: resource.Timeout{Read: readTimeout},
	}
	builder := &Builder{}
	handler, err := builder.buildReverseHandler(resource.Route{Upstream: upstream}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}
	t.Cleanup(builder.Stop)
	return handler
}

func TestProxyFaultHandling(t *testing.T) {
	fixture := newFaultUpstream()
	upstreamServer := httptest.NewServer(fixture)
	defer upstreamServer.Close()

	gateway := httptest.NewServer(buildFaultHandler(t, upstreamServer.URL, 1, 1))
	defer gateway.Close()
	// Fresh connections per request so the client transport never auto-retries
	// an aborted connection and sub-tests do not share pooled connections.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	tests := []struct {
		name         string
		method       string
		idempotency  string
		mode         upstreamFaultMode
		wantStatus   int
		wantAttempts int
		wantAborted  bool
	}{
		{
			name:         "GET retries reset",
			method:       http.MethodGet,
			mode:         faultReset,
			wantStatus:   http.StatusBadGateway,
			wantAttempts: 2,
		},
		{
			name:         "POST does not retry reset",
			method:       http.MethodPost,
			mode:         faultReset,
			wantStatus:   http.StatusBadGateway,
			wantAttempts: 1,
		},
		{
			name:         "keyed POST retries reset",
			method:       http.MethodPost,
			idempotency:  "order-123",
			mode:         faultReset,
			wantStatus:   http.StatusBadGateway,
			wantAttempts: 2,
		},
		{
			name:         "header inactivity maps to 504",
			method:       http.MethodGet,
			mode:         faultHeaderTimeout,
			wantStatus:   http.StatusGatewayTimeout,
			wantAttempts: 2,
		},
		{
			name:         "body inactivity terminates copy",
			method:       http.MethodGet,
			mode:         faultBodyStall,
			wantStatus:   http.StatusOK,
			wantAttempts: 1,
			wantAborted:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture.mode.Store(test.mode)
			fixture.attempts.Store(0)

			request, err := http.NewRequest(test.method, gateway.URL+"/fault", nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if test.idempotency != "" {
				request.Header.Set("Idempotency-Key", test.idempotency)
			}
			if test.wantAborted {
				// The upstream committed 200 but stalled the body. Go's
				// ReverseProxy aborts the connection on a body-copy error
				// (ErrAbortHandler), so the client observes a transport error
				// rather than a truncated response; the committed status is
				// never rewritten to 504 and the copy terminates within the
				// read timeout.
				done := make(chan error, 1)
				go func() {
					response, doErr := client.Do(request)
					if response != nil {
						_, _ = io.Copy(io.Discard, response.Body)
						_ = response.Body.Close()
					}
					done <- doErr
				}()
				select {
				case doErr := <-done:
					if doErr == nil {
						t.Fatal("client.Do() error = nil, want an aborted connection")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("body-stall request did not terminate within the read timeout")
				}
				if got := int(fixture.attempts.Load()); got != test.wantAttempts {
					t.Fatalf("upstream attempts = %d, want %d", got, test.wantAttempts)
				}
				return
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("client.Do() error = %v", err)
			}
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if got := int(fixture.attempts.Load()); got != test.wantAttempts {
				t.Fatalf("upstream attempts = %d, want %d", got, test.wantAttempts)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		})
	}
}

func TestProxyFaultOverloadRecovers(t *testing.T) {
	previous := config.GlobalConfig
	config.GlobalConfig = &config.Config{Proxy: config.Proxy{MaxInFlight: 1}}
	t.Cleanup(func() { config.GlobalConfig = previous })

	release := make(chan struct{})
	var firstHeldOnce sync.Once
	firstHeld := make(chan struct{})
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		firstHeldOnce.Do(func() { close(firstHeld) })
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstreamServer.Close()

	handler := buildFaultHandler(t, upstreamServer.URL, 0, 0)

	// First request: the upstream commits 200 and stalls its body, so the
	// response copy holds the single admission token open. The fixture closes
	// firstHeld once its headers are on the wire, which deterministically
	// precedes any later admission check.
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, httptest.NewRequest(http.MethodGet, "http://gateway.test/fault", nil))
		close(firstDone)
	}()
	select {
	case <-firstHeld:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the upstream response")
	}

	// Second request is rejected by the admission limit while the first body
	// is still open.
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, httptest.NewRequest(http.MethodGet, "http://gateway.test/fault", nil))
	if secondResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want 503", secondResponse.Code)
	}

	// Release the held body; the admission token returns and traffic resumes.
	close(release)
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish after the upstream released its body")
	}
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", firstResponse.Code)
	}

	thirdResponse := httptest.NewRecorder()
	handler.ServeHTTP(thirdResponse, httptest.NewRequest(http.MethodGet, "http://gateway.test/fault", nil))
	if thirdResponse.Code != http.StatusOK {
		t.Fatalf("third status = %d, want 200", thirdResponse.Code)
	}
}

func TestProxyFaultActiveHealthRecovers(t *testing.T) {
	var recovered atomic.Bool
	var failingRouteHits atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			failingRouteHits.Add(1)
		}
		if recovered.Load() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer healthy.Close()

	upstream := resource.Upstream{
		Scheme: "http",
		Nodes: []resource.Node{
			upstreamNode(t, failing.URL),
			upstreamNode(t, healthy.URL),
		},
		Checks: map[string]any{
			"active": map[string]any{
				"http_path": "/healthz",
				"healthy": map[string]any{
					"interval":  1,
					"successes": 2,
				},
				"unhealthy": map[string]any{
					"interval":      1,
					"http_failures": 1,
				},
			},
		},
	}
	builder := &Builder{}
	handler, err := builder.buildReverseHandler(resource.Route{Upstream: upstream}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}
	t.Cleanup(builder.Stop)

	gateway := httptest.NewServer(handler)
	defer gateway.Close()
	client := gateway.Client()

	// Wait until active probes quarantine the failing node: a burst of routed
	// requests must stop reaching it. The node may already be quarantined by
	// the first probe, so a stable burst is the signal, not a prior hit.
	quarantineDeadline := time.Now().Add(10 * time.Second)
	for {
		before := failingRouteHits.Load()
		for range 8 {
			_ = requestFaultStatus(client, gateway.URL)
		}
		if failingRouteHits.Load() == before {
			break
		}
		if time.Now().After(quarantineDeadline) {
			t.Fatalf("failing node was never quarantined (routed hits = %d)", failingRouteHits.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	quarantinedHits := failingRouteHits.Load()

	for range 10 {
		if status := requestFaultStatus(client, gateway.URL); status != http.StatusNoContent {
			t.Fatalf("quarantined request status = %d, want 204", status)
		}
	}
	if got := failingRouteHits.Load(); got != quarantinedHits {
		t.Fatalf("routed hits while quarantined = %d, want %d", got, quarantinedHits)
	}

	// The node recovers; after the configured consecutive successes it must
	// re-enter weighted selection.
	recovered.Store(true)
	recoveryDeadline := time.Now().Add(10 * time.Second)
	for {
		_ = requestFaultStatus(client, gateway.URL)
		if failingRouteHits.Load() > quarantinedHits {
			return
		}
		if time.Now().After(recoveryDeadline) {
			t.Fatal("recovered node never re-entered selection")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func requestFaultStatus(client *http.Client, baseURL string) int {
	response, err := client.Get(baseURL + "/fault")
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode
}
