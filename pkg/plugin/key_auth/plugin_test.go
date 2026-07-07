package key_auth

import (
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
		s := store.NewStore(t.TempDir()+"/key-auth.db", testEvents)
		s.Start()
	})
}

func addKeyAuthConsumer(t *testing.T, username, key string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"key-auth": map[string]any{
				"key": key,
			},
		},
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumerByPluginKey(name, key); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for key-auth key %q", username, key)
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

func TestHandlerAcceptsHeaderKeyAndAttachesConsumer(t *testing.T) {
	addKeyAuthConsumer(t, "key-user", "header-key")
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("apikey", "header-key")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "key-user" {
			t.Fatalf("consumer_name = %v, want key-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerRejectsMissingKey(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(rr.Body.String(), "Missing API key found in request") {
		t.Fatalf("body = %q, want missing key message", rr.Body.String())
	}
}

func TestHandlerRejectsInvalidKey(t *testing.T) {
	addKeyAuthConsumer(t, "valid-key-user", "valid-key")
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("apikey", "wrong-key")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(rr.Body.String(), "Invalid API key in request") {
		t.Fatalf("body = %q, want invalid key message", rr.Body.String())
	}
}

func TestHandlerHideCredentialsRemovesQueryKey(t *testing.T) {
	addKeyAuthConsumer(t, "query-user", "query-key")
	hideCredentials := true
	p := newTestPlugin(t, Config{HideCredentials: &hideCredentials})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?apikey=query-key&keep=1", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("apikey"); got != "" {
			t.Fatalf("apikey query = %q, want removed", got)
		}
		if got := r.URL.Query().Get("keep"); got != "1" {
			t.Fatalf("keep query = %q, want preserved", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}
