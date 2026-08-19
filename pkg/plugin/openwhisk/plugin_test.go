package openwhisk

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/util"
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
		_, _ = w.Write([]byte(`{"statusCode":202,"headers":{"X-Action":"done"},"body":"action body"}`))
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

func TestRunRequestPhasePublishesUpstreamSource(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":204}`))
	}))
	defer api.Close()
	p := newTestPlugin(t, Config{APIHost: api.URL, ServiceToken: "token", Namespace: "guest", Action: "hello"})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/openwhisk", nil)
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
}

func TestHandlerReturnsServiceUnavailableForInvalidOpenWhiskJSON(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
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

func TestHandlerRelaysScalarAndListResultHeaders(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"headers":{"X-Rate-Limit":7,"X-Values":["one","two"]},"body":{"ok":true}}`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})
	res := performRequest(p, "")

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Header().Get("X-Rate-Limit"); got != "7" {
		t.Fatalf("X-Rate-Limit = %q, want 7", got)
	}
	if got := res.Header().Values("X-Values"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("X-Values = %#v, want [one two]", got)
	}
	if got := res.Body.String(); got != `{"ok":true}` {
		t.Fatalf("response body = %q, want JSON object", got)
	}
}

func TestSchemaRejectsInvalidOpenWhiskNames(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"api_host":      "https://openwhisk.example",
		"service_token": "user:pass",
		"namespace":     "bad/name",
		"action":        "hello",
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("Validate() error = nil, want invalid namespace rejected")
	}
}

func TestWriteActionResponseDropsConnectionHeadersForHTTP2(t *testing.T) {
	p := &Plugin{}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			`{"statusCode":200,"headers":{"Connection":"keep-alive","Keep-Alive":"timeout=5","Proxy-Connection":"keep-alive","Upgrade":"websocket","Transfer-Encoding":"chunked","X-Result":"ok"},"body":"done"}`,
		)),
	}
	recorder := httptest.NewRecorder()

	p.writeActionResponse(recorder, response, true)

	for _, field := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Upgrade", "Transfer-Encoding"} {
		if got := recorder.Header().Get(field); got != "" {
			t.Fatalf("%s = %q, want removed", field, got)
		}
	}
	if got := recorder.Header().Get("X-Result"); got != "ok" {
		t.Fatalf("X-Result = %q, want ok", got)
	}
	if got := recorder.Body.String(); got != "done" {
		t.Fatalf("body = %q, want done", got)
	}
}

func TestWriteActionResponseFallsBackToOriginalJSONForFalseBody(t *testing.T) {
	original := `{"statusCode":202,"headers":{"X-Action":"done"},"body":false}`
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(original)),
	}
	recorder := httptest.NewRecorder()

	(&Plugin{}).writeActionResponse(recorder, response, false)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if got := recorder.Header().Get("X-Action"); got != "done" {
		t.Fatalf("X-Action = %q, want done", got)
	}
	if got := recorder.Body.String(); got != original {
		t.Fatalf("body = %q, want original JSON %q", got, original)
	}
}

func TestHandlerHonorsDisabledSSLVerify(t *testing.T) {
	api := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":201,"body":"tls ok"}`))
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
		_, _ = w.Write([]byte(`{"statusCode":204}`))
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
