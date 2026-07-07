package function_upstream

import (
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandlerHonorsDisabledSSLVerify(t *testing.T) {
	function := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("function ok"))
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

	transport, ok := p.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", p.Client.Transport)
	}
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

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{Config: cfg}
	p.Name = "function-upstream"
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
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
