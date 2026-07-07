package authz_casbin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
