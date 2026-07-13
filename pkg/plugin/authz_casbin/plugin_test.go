package authz_casbin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	projectjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/store"
)

var (
	metadataStoreOnce   sync.Once
	metadataStoreEvents chan *store.Event
)

const testModel = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && r.obj == p.obj && r.act == p.act
`

const testPolicy = `
p, alice, /orders/123, GET
p, anonymous, /public, GET
`

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

func TestHandlerAllowsRequestWhenPolicyMatchesHeaderUser(t *testing.T) {
	p := newTestPlugin(t, Config{
		Model:    testModel,
		Policy:   testPolicy,
		Username: "X-User",
	})

	called := false
	req := httptest.NewRequest(http.MethodGet, "/orders/123", nil)
	req.Header.Set("X-User", "alice")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called for an allowed request")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsRequestWhenPolicyDoesNotMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		Model:    testModel,
		Policy:   testPolicy,
		Username: "X-User",
	})

	req := httptest.NewRequest(http.MethodPost, "/orders/123", nil)
	req.Header.Set("X-User", "alice")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for a denied request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Access Denied") {
		t.Fatalf("body = %q, want Access Denied message", rr.Body.String())
	}
}

func TestHandlerUsesAnonymousWhenUsernameHeaderIsMissing(t *testing.T) {
	p := newTestPlugin(t, Config{
		Model:    testModel,
		Policy:   testPolicy,
		Username: "X-User",
	})

	called := false
	req := httptest.NewRequest(http.MethodGet, "/public", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called for anonymous policy match")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
}

func TestPostInitLoadsModelAndPolicyFromPaths(t *testing.T) {
	dir := t.TempDir()
	modelPath := dir + "/model.conf"
	policyPath := dir + "/policy.csv"

	if err := os.WriteFile(modelPath, []byte(testModel), 0o600); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if err := os.WriteFile(policyPath, []byte(testPolicy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	p := newTestPlugin(t, Config{
		ModelPath:  modelPath,
		PolicyPath: policyPath,
		Username:   "X-User",
	})

	req := httptest.NewRequest(http.MethodGet, "/orders/123", nil)
	req.Header.Set("X-User", "alice")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerLoadsCasbinModelAndPolicyFromPluginMetadata(t *testing.T) {
	putCasbinMetadata(t, testModel, testPolicy)
	p := newTestPlugin(t, Config{Username: "X-User"})

	req := httptest.NewRequest(http.MethodGet, "/orders/123", nil)
	req.Header.Set("X-User", "alice")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerReloadsCasbinPluginMetadata(t *testing.T) {
	putCasbinMetadata(t, testModel, `p, alice, /orders/123, GET`)
	p := newTestPlugin(t, Config{Username: "X-User"})

	first := httptest.NewRequest(http.MethodGet, "/orders/123", nil)
	first.Header.Set("X-User", "alice")
	firstRecorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("initial status = %d, want 204", firstRecorder.Code)
	}

	putCasbinMetadata(t, testModel, `p, bob, /orders/123, GET`)
	updated := httptest.NewRequest(http.MethodGet, "/orders/123", nil)
	updated.Header.Set("X-User", "bob")
	updatedRecorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(updatedRecorder, updated)

	if updatedRecorder.Code != http.StatusAccepted {
		t.Fatalf("updated status = %d, want 202; body=%s", updatedRecorder.Code, updatedRecorder.Body.String())
	}
}

func putCasbinMetadata(t *testing.T, modelText, policyText string) {
	t.Helper()
	metadataStoreOnce.Do(func() {
		metadataStoreEvents = make(chan *store.Event, 8)
		s := store.NewStore(t.TempDir()+"/authz-casbin.db", metadataStoreEvents)
		s.Start()
	})

	body, err := projectjson.Marshal(map[string]string{"model": modelText, "policy": policyText})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	metadataStoreEvents <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/plugin_metadata/authz-casbin"),
		Value: body,
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var metadata Metadata
		if err := store.GetPluginMetadata(name, &metadata); err == nil &&
			metadata.Model == modelText && metadata.Policy == policyText {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for authz-casbin plugin metadata")
}
