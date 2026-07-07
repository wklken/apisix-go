package forward_auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		w.Write([]byte("nope"))
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

func performRequest(p *Plugin, upstream func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?x=1", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(upstream)).ServeHTTP(rr, req)
	return rr
}
