package openwhisk

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerInvokesOpenWhiskActionAndUsesJSONResult(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuthorization, gotContentType, gotBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuthorization = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read action request body: %v", err)
		}
		gotBody = string(body)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"statusCode":202,"headers":{"X-Action":"done"},"body":"action body"}`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Package:      "samples",
		Action:       "hello",
	})

	res := performRequest(p, "payload")

	if res.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusAccepted)
	}
	if got := res.Body.String(); got != "action body" {
		t.Fatalf("response body = %q, want action body", got)
	}
	if got := res.Header().Get("X-Action"); got != "done" {
		t.Fatalf("X-Action = %q, want done", got)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("action method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/namespaces/guest/actions/samples/hello" {
		t.Fatalf("action path = %q, want OpenWhisk action endpoint", gotPath)
	}
	if gotQuery != "blocking=true&result=true&timeout=3000" {
		t.Fatalf("action query = %q, want blocking=true&result=true&timeout=3000", gotQuery)
	}
	if gotAuthorization != "Basic dXNlcjpwYXNz" {
		t.Fatalf("Authorization = %q, want Basic dXNlcjpwYXNz", gotAuthorization)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody != "payload" {
		t.Fatalf("action body = %q, want payload", gotBody)
	}
}

func TestHandlerReturnsServiceUnavailableForInvalidOpenWhiskJSON(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not-json`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})

	res := performRequest(p, "")

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestHandlerHonorsDisabledSSLVerify(t *testing.T) {
	api := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"statusCode":201,"body":"tls ok"}`))
	}))
	defer api.Close()

	sslVerify := false
	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		SSLVerify:    &sslVerify,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})

	res := performRequest(p, "")

	if res.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d, body=%q", res.Code, http.StatusCreated, res.Body.String())
	}
	if got := res.Body.String(); got != "tls ok" {
		t.Fatalf("response body = %q, want tls ok", got)
	}
}

func TestHandlerRejectsSelfSignedAPIWhenSSLVerifyDefaultsTrue(t *testing.T) {
	api := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"statusCode":204}`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})

	res := performRequest(p, "")

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestPostInitAppliesKeepaliveTransportOptions(t *testing.T) {
	sslVerify := false
	keepalive := false
	p := newTestPlugin(t, Config{
		APIHost:          "https://openwhisk.example",
		SSLVerify:        &sslVerify,
		ServiceToken:     "user:pass",
		Namespace:        "guest",
		Action:           "hello",
		Keepalive:        &keepalive,
		KeepaliveTimeout: 1500,
		KeepalivePool:    7,
	})

	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", p.client.Transport)
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

func performRequest(p *Plugin, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "http://example.com/hello?client=ignored", strings.NewReader(body))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := http.StatusInternalServerError
		http.Error(w, http.StatusText(t), t)
	})).ServeHTTP(rr, req)
	return rr
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
