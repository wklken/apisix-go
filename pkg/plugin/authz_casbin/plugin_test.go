package authz_casbin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	projectjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestSchemaAcceptsPluginMetadataConfiguration(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{"username": "user"}, p.GetSchema()); err != nil {
		t.Fatalf("metadata-backed config should validate: %v", err)
	}
}

func TestSchemaRejectsConfigurationWithoutUsername(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{
		"model_path":  "/tmp/model.conf",
		"policy_path": "/tmp/policy.csv",
	}, p.GetSchema())
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("config without username error = %v, want identifying username diagnostic", err)
	}
}

func TestSchemaAllowsCompleteFilePairWithStrayInlineModel(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"model_path":  "/tmp/model.conf",
		"policy_path": "/tmp/policy.csv",
		"model":       "stray incomplete inline model",
		"username":    "user",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("complete file pair with stray inline model should validate: %v", err)
	}
}

func TestSchemaAllowsCompleteInlinePairWithStrayModelPath(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"model":      testModel,
		"policy":     testPolicy,
		"model_path": "/tmp/stray-model.conf",
		"username":   "user",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("complete inline pair with stray model_path should validate: %v", err)
	}
}

func TestSchemaRejectsTwoCompleteConfigurationPairs(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"model_path":  "/tmp/model.conf",
		"policy_path": "/tmp/policy.csv",
		"model":       testModel,
		"policy":      testPolicy,
		"username":    "user",
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("two complete configuration pairs should fail oneOf validation")
	}
}

func TestMetadataSchemaRequiresCasbinModelAndPolicy(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, metadata := range []map[string]any{
		{},
		{"model": testModel},
		{"policy": testPolicy},
		{"model": "", "policy": testPolicy},
		{"model": testModel, "policy": ""},
	} {
		if err := util.Validate(metadata, p.GetMetadataSchema()); err == nil {
			t.Fatalf("metadata %#v validated, want required non-empty model and policy", metadata)
		}
	}
	if err := util.Validate(map[string]any{
		"model":  testModel,
		"policy": testPolicy,
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("complete metadata should validate: %v", err)
	}
}

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

func newTestPluginWithMetadata(t *testing.T, cfg Config, modelText, policyText string) *Plugin {
	t.Helper()

	document, err := projectjson.Marshal(map[string]string{"model": modelText, "policy": policyText})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	metadata, err := runtime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Metadata: metadata})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func serveRequest(t *testing.T, p *Plugin, user string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/orders/123", nil)
	req.Header.Set("X-User", user)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	return rr.Code
}

func TestPreparedGenerationsRetainCasbinMetadata(t *testing.T) {
	policyAlice := "p, alice, /orders/123, GET"
	policyBob := "p, bob, /orders/123, GET"
	pluginN := newTestPluginWithMetadata(t, Config{Username: "X-User"}, testModel, policyAlice)
	pluginNPlusOne := newTestPluginWithMetadata(t, Config{Username: "X-User"}, testModel, policyBob)

	for _, test := range []struct {
		name   string
		plugin *Plugin
		user   string
		want   int
	}{
		{name: "N allows Alice", plugin: pluginN, user: "alice", want: http.StatusNoContent},
		{name: "N forbids Bob", plugin: pluginN, user: "bob", want: http.StatusForbidden},
		{name: "N+1 forbids Alice", plugin: pluginNPlusOne, user: "alice", want: http.StatusForbidden},
		{name: "N+1 allows Bob", plugin: pluginNPlusOne, user: "bob", want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := serveRequest(t, test.plugin, test.user); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRouteCasbinConfigOverridesPreparedMetadata(t *testing.T) {
	routePolicy := "p, alice, /orders/123, GET"
	metadataPolicy := "p, bob, /orders/123, GET"
	p := newTestPluginWithMetadata(t, Config{
		Model:    testModel,
		Policy:   routePolicy,
		Username: "X-User",
	}, testModel, metadataPolicy)

	if got := serveRequest(t, p, "alice"); got != http.StatusNoContent {
		t.Fatalf("route policy status for Alice = %d, want %d", got, http.StatusNoContent)
	}
	if got := serveRequest(t, p, "bob"); got != http.StatusForbidden {
		t.Fatalf("route policy status for Bob = %d, want %d", got, http.StatusForbidden)
	}
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
	if got := rr.Body.String(); got != "{\"message\":\"Access Denied\"}\n" {
		t.Fatalf("body = %q, want APISIX Access Denied response", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX response type", got)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "" {
		t.Fatalf("X-Content-Type-Options = %q, want absent", got)
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

func TestConcurrentEnforceUsesPreparedGenerations(t *testing.T) {
	pluginN := newTestPluginWithMetadata(t, Config{Username: "X-User"}, testModel,
		"p, alice, /orders/123, GET")
	pluginNPlusOne := newTestPluginWithMetadata(t, Config{Username: "X-User"}, testModel,
		"p, bob, /orders/123, GET")

	var wg sync.WaitGroup
	errs := make(chan error, 8*128)
	for _, test := range []struct {
		plugin *Plugin
		user   string
		want   int
	}{
		{plugin: pluginN, user: "alice", want: http.StatusNoContent},
		{plugin: pluginN, user: "bob", want: http.StatusForbidden},
		{plugin: pluginNPlusOne, user: "alice", want: http.StatusForbidden},
		{plugin: pluginNPlusOne, user: "bob", want: http.StatusNoContent},
	} {
		for range 2 {
			wg.Go(func() {
				for range 128 {
					if got := serveRequest(t, test.plugin, test.user); got != test.want {
						errs <- fmt.Errorf("user %s status = %d, want %d", test.user, got, test.want)
					}
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
