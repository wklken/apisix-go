package forward_auth

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestSchemaAcceptsSSLVerifyAndKeepaliveOptions(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":               "https://auth.example.com/check",
		"ssl_verify":        false,
		"keepalive":         false,
		"keepalive_timeout": 1500,
		"keepalive_pool":    7,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("forward-auth config should validate: %v", err)
	}
}

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

func TestHandlerAllowsRequestAndCopiesUpstreamHeaders(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("Authorization = %q, want Bearer token", got)
		}
		if got := r.Header.Get("X-Forwarded-Method"); got != http.MethodGet {
			t.Fatalf("X-Forwarded-Method = %q, want GET", got)
		}
		if got := r.Header.Get("X-Forwarded-Uri"); got != "/get?x=1" {
			t.Fatalf("X-Forwarded-Uri = %q, want /get?x=1", got)
		}
		if got := r.Header.Get("X-Extra"); got != "static" {
			t.Fatalf("X-Extra = %q, want static", got)
		}

		w.Header().Set("X-User-ID", "alice")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer auth.Close()

	p := newTestPlugin(t, Config{
		URI:             auth.URL,
		RequestHeaders:  []string{"Authorization"},
		ExtraHeaders:    map[string]string{"X-Extra": "static"},
		UpstreamHeaders: []string{"X-User-ID"},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-User-ID"); got != "alice" {
			t.Fatalf("X-User-ID = %q, want alice", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerResolvesExtraHeaderVariables(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Client-IP"); got != "192.0.2.10" {
			t.Fatalf("X-Client-IP = %q, want 192.0.2.10", got)
		}
		if got := r.Header.Get("X-Original-URI"); got != "/get?x=1" {
			t.Fatalf("X-Original-URI = %q, want /get?x=1", got)
		}
		if got := r.Header.Get("X-Static"); got != "prefix-192.0.2.10" {
			t.Fatalf("X-Static = %q, want prefix-192.0.2.10", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer auth.Close()

	p := newTestPlugin(t, Config{
		URI: auth.URL,
		ExtraHeaders: map[string]string{
			"X-Client-IP":    "$remote_addr",
			"X-Original-URI": "$request_uri",
			"X-Static":       "prefix-$remote_addr",
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsAndCopiesClientHeaders(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Forward-Auth", "deny")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("nope"))
	}))
	defer auth.Close()

	p := newTestPlugin(t, Config{
		URI:           auth.URL,
		ClientHeaders: []string{"X-Forward-Auth"},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if got := res.Header().Get("X-Forward-Auth"); got != "deny" {
		t.Fatalf("X-Forward-Auth = %q, want deny", got)
	}
	if got := res.Body.String(); got != "nope" {
		t.Fatalf("body = %q, want nope", got)
	}
}

func TestHandlerUsesStatusOnError(t *testing.T) {
	p := newTestPlugin(t, Config{
		URI:           "http://127.0.0.1:1/auth",
		StatusOnError: http.StatusBadGateway,
		Timeout:       1,
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})

	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
}

func TestHandlerPostForwardsBodyAndRestoresRequestBody(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read auth body: %v", err)
		}
		if got := string(body); got != "payload" {
			t.Fatalf("auth body = %q, want payload", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer auth.Close()

	p := newTestPlugin(t, Config{
		URI:           auth.URL,
		RequestMethod: http.MethodPost,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/get", strings.NewReader("payload"))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerPostForwardsRequestBodyTransportHeaders(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		if got := r.Header.Get("Expect"); got != "100-continue" {
			t.Fatalf("Expect = %q, want 100-continue", got)
		}
		if got := r.Header.Get("Content-Length"); got != "7" {
			t.Fatalf("Content-Length = %q, want 7", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(auth.Close)

	p := newTestPlugin(t, Config{
		URI:           auth.URL,
		RequestMethod: http.MethodPost,
	})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/get", strings.NewReader("payload"))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Expect", "100-continue")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerHonorsDisabledSSLVerify(t *testing.T) {
	auth := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer auth.Close()

	verify := false
	p := newTestPlugin(t, Config{
		URI:       auth.URL,
		SSLVerify: &verify,
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsSelfSignedAuthWhenSSLVerifyDefaultsTrue(t *testing.T) {
	auth := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer auth.Close()

	p := newTestPlugin(t, Config{
		URI:           auth.URL,
		StatusOnError: http.StatusBadGateway,
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})

	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
}

func TestPostInitAppliesKeepaliveTransportOptions(t *testing.T) {
	keepalive := false
	verify := false
	p := newTestPlugin(t, Config{
		URI:              "https://auth.example.com/check",
		SSLVerify:        &verify,
		Keepalive:        &keepalive,
		KeepaliveTimeout: 1500,
		KeepalivePool:    7,
	})

	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", p.client.Transport)
	}
	if !transport.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = false, want true")
	}
	if transport.MaxIdleConnsPerHost != 7 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 7", transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != 1500*time.Millisecond {
		t.Fatalf("IdleConnTimeout = %s, want 1.5s", transport.IdleConnTimeout)
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("TLSClientConfig = %#v, want InsecureSkipVerify", transport.TLSClientConfig)
	}
}

func TestPostInitDefaultsSSLVerifyAndKeepalive(t *testing.T) {
	p := newTestPlugin(t, Config{
		URI: "https://auth.example.com/check",
	})

	if p.config.SSLVerify == nil || !*p.config.SSLVerify {
		t.Fatalf("SSLVerify = %v, want true", p.config.SSLVerify)
	}
	if p.config.Keepalive == nil || !*p.config.Keepalive {
		t.Fatalf("Keepalive = %v, want true", p.config.Keepalive)
	}
	if p.config.KeepaliveTimeout != 60000 {
		t.Fatalf("KeepaliveTimeout = %d, want 60000", p.config.KeepaliveTimeout)
	}
	if p.config.KeepalivePool != 5 {
		t.Fatalf("KeepalivePool = %d, want 5", p.config.KeepalivePool)
	}
	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", p.client.Transport)
	}
	if transport.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = true, want false")
	}
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify = true, want false")
	}
}

func performRequest(p *Plugin, upstream func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?x=1", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(upstream)).ServeHTTP(rr, req)
	return rr
}

func newQuietTLSServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	return server
}
