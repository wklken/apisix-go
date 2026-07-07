package wolf_rbac

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	projectjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/store"
)

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s := store.NewStore(t.TempDir()+"/wolf-rbac.db", testEvents)
		s.Start()
	})
}

func addWolfConsumer(t *testing.T, username, appid, server string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"wolf-rbac": map[string]any{
				"appid":         appid,
				"server":        server,
				"header_prefix": "X-",
				"ssl_verify":    false,
			},
		},
	}
	body, err := projectjson.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/" + username)
	event.Value = body
	testEvents <- event

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumerByPluginKey("wolf-rbac", appid); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for wolf-rbac appid %q", username, appid)
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

func TestHandlerChecksWolfPermissionAndAttachesConsumer(t *testing.T) {
	requests := make(chan *http.Request, 1)
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		if r.URL.Path != "/wolf/rbac/access_check" {
			t.Fatalf("path = %q, want access_check", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"userInfo": map[string]any{
					"id":       "u-1",
					"username": "alice",
					"nickname": "Alice Zhang",
				},
			},
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-user", "app-a", wolf.URL)
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders/1?debug=true", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("Authorization", "V1#app-a#wolf-token")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "wolf-user" {
			t.Fatalf("consumer_name = %v, want wolf-user", got)
		}
		if got := r.Header.Get("X-UserId"); got != "u-1" {
			t.Fatalf("upstream X-UserId = %q, want u-1", got)
		}
		if got := r.Header.Get("X-Username"); got != "alice" {
			t.Fatalf("upstream X-Username = %q, want alice", got)
		}
		if got := r.Header.Get("X-Nickname"); got != url.QueryEscape("Alice Zhang") {
			t.Fatalf("upstream X-Nickname = %q, want escaped nickname", got)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-UserId") != "u-1" {
		t.Fatalf("response X-UserId = %q, want u-1", rr.Header().Get("X-UserId"))
	}

	select {
	case got := <-requests:
		query := got.URL.Query()
		if query.Get("appID") != "app-a" {
			t.Fatalf("appID = %q, want app-a", query.Get("appID"))
		}
		if query.Get("resName") != "/orders/1" {
			t.Fatalf("resName = %q, want path", query.Get("resName"))
		}
		if query.Get("action") != http.MethodPost {
			t.Fatalf("action = %q, want POST", query.Get("action"))
		}
		if query.Get("clientIP") != "203.0.113.10" {
			t.Fatalf("clientIP = %q, want remote IP", query.Get("clientIP"))
		}
		if got.Header.Get("X-Rbac-Token") != "wolf-token" {
			t.Fatalf("x-rbac-token = %q, want wolf-token", got.Header.Get("X-Rbac-Token"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Wolf access_check request")
	}
}

func TestHandlerRejectsMissingAndInvalidToken(t *testing.T) {
	p := newTestPlugin(t, Config{})

	missing := performRequest(t, p, "")
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", missing.Code)
	}
	if !strings.Contains(missing.Body.String(), "Missing rbac token") {
		t.Fatalf("missing token body = %q", missing.Body.String())
	}

	invalid := performRequest(t, p, "invalid-token")
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", invalid.Code)
	}
	if !strings.Contains(invalid.Body.String(), "invalid rbac token") {
		t.Fatalf("invalid token body = %q", invalid.Body.String())
	}
}

func TestHandlerPropagatesWolfDenial(t *testing.T) {
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"reason": "permission denied",
		})
	}))
	t.Cleanup(wolf.Close)
	addWolfConsumer(t, "wolf-denied-user", "app-denied", wolf.URL)
	p := newTestPlugin(t, Config{})

	res := performRequest(t, p, "V1#app-denied#wolf-token")
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if !strings.Contains(res.Body.String(), "permission denied") {
		t.Fatalf("body = %q, want denial reason", res.Body.String())
	}
}

func TestFetchTokenFromQueryAndCookie(t *testing.T) {
	if got := fetchRBACToken(httptest.NewRequest(http.MethodGet, "/?rbac_token=V1%23app%23query", nil)); got != "V1#app#query" {
		t.Fatalf("query token = %q", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "x-rbac-token", Value: "V1#app#cookie"})
	if got := fetchRBACToken(req); got != "V1#app#cookie" {
		t.Fatalf("cookie token = %q", got)
	}
}

func performRequest(t *testing.T, p *Plugin, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)
	return rr
}
