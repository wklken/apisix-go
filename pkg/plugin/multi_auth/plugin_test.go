package multi_auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
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
		s := store.NewStore(t.TempDir()+"/multi-auth.db", testEvents)
		s.Start()
	})
}

func addAuthConsumer(t *testing.T, username string, plugins map[string]any) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins":  plugins,
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/" + username)
	event.Value = body
	testEvents <- event
}

func waitForConsumerKey(t *testing.T, pluginName string, key string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumerByPluginKey(pluginName, key); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer key %s:%s was not indexed", pluginName, key)
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

func TestHandlerAllowsRequestWhenAnyAuthPluginSucceeds(t *testing.T) {
	addAuthConsumer(t, "key-user", map[string]any{
		"key-auth": map[string]any{"key": "valid-key"},
	})
	waitForConsumerKey(t, "key-auth", "valid-key")

	hideCredentials := true
	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"basic-auth": {}},
			{"key-auth": {"hide_credentials": hideCredentials, "header": "apikey"}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "valid-key")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "key-user" {
			t.Fatalf("consumer_name = %v, want key-user", got)
		}
		if got := r.Header.Get("apikey"); got != "" {
			t.Fatalf("apikey header = %q, want hidden", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerAllowsBasicAuthWhenLaterPluginWouldFail(t *testing.T) {
	addAuthConsumer(t, "basic-user", map[string]any{
		"basic-auth": map[string]any{"username": "basic-user", "password": "secret"},
	})
	waitForConsumerKey(t, "basic-auth", "basic-user")

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"key-auth": {}},
			{"basic-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("basic-user:secret")))
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "basic-user" {
			t.Fatalf("consumer_name = %v, want basic-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsWhenAllAuthPluginsFail(t *testing.T) {
	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"basic-auth": {}},
			{"key-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", res.Code)
	}
	if !strings.Contains(res.Body.String(), "Authorization Failed") {
		t.Fatalf("body = %q, want Authorization Failed", res.Body.String())
	}
}

func TestPostInitRejectsUnsupportedAuthPlugin(t *testing.T) {
	p := &Plugin{config: Config{
		AuthPlugins: []AuthPluginConfig{
			{"key-auth": {}},
			{"unknown-auth": {}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "unknown-auth") {
		t.Fatalf("PostInit() error = %v, want unknown-auth", err)
	}
}

func newMultiAuthRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req = ctx.WithRequestVars(req)
	return req
}
