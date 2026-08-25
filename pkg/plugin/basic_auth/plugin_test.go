package basic_auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

type basicAuthConsumerLookup struct {
	mu      sync.RWMutex
	byKey   map[string]resource.Consumer
	byID    map[string]resource.Consumer
	keyCall []string
	idCall  []string
	closed  bool
}

func (lookup *basicAuthConsumerLookup) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.keyCall = append(lookup.keyCall, plugin+"\x00"+key)
	if lookup.closed {
		return resource.Consumer{}, false
	}
	consumer, ok := lookup.byKey[key]
	return consumer, ok
}

func (lookup *basicAuthConsumerLookup) ConsumerByID(id string) (resource.Consumer, bool) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.idCall = append(lookup.idCall, id)
	if lookup.closed {
		return resource.Consumer{}, false
	}
	consumer, ok := lookup.byID[id]
	return consumer, ok
}

func (*basicAuthConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func (lookup *basicAuthConsumerLookup) close() {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.closed = true
	lookup.byKey = nil
	lookup.byID = nil
}

func basicAuthBoundConsumer(lookupUsername, consumerUsername, password string) resource.Consumer {
	return resource.Consumer{
		Username: consumerUsername,
		Plugins: map[string]resource.PluginConfig{name: map[string]any{
			"username": lookupUsername,
			"password": password,
		}},
	}
}

func newLookupTestPlugin(t *testing.T, cfg Config, lookup base.ConsumerLookup) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p
}

func basicAuthLookup(consumers ...resource.Consumer) *basicAuthConsumerLookup {
	lookup := &basicAuthConsumerLookup{
		byKey: make(map[string]resource.Consumer, len(consumers)),
		byID:  make(map[string]resource.Consumer, len(consumers)),
	}
	for _, consumer := range consumers {
		lookup.byID[consumer.Username] = consumer
		config, ok := consumer.Plugins[name].(map[string]any)
		if !ok {
			continue
		}
		username, ok := config["username"].(string)
		if ok {
			lookup.byKey[username] = consumer
		}
	}
	return lookup
}

type basicAuthTestFixture struct {
	sync.Mutex
	consumers []resource.Consumer
}

var basicAuthTestFixtures sync.Map

func basicAuthFixtureFor(t *testing.T) *basicAuthTestFixture {
	t.Helper()
	fixture := &basicAuthTestFixture{}
	actual, loaded := basicAuthTestFixtures.LoadOrStore(t, fixture)
	if !loaded {
		t.Cleanup(func() { basicAuthTestFixtures.Delete(t) })
	}
	return actual.(*basicAuthTestFixture)
}

func addBasicAuthConsumer(t *testing.T, username, password string) {
	t.Helper()
	fixture := basicAuthFixtureFor(t)
	fixture.Lock()
	defer fixture.Unlock()
	fixture.consumers = append(
		fixture.consumers,
		basicAuthBoundConsumer(username, username, password),
	)
}

func newTestPlugin(t *testing.T, cfg Config, consumers ...resource.Consumer) *Plugin {
	t.Helper()

	fixture := basicAuthFixtureFor(t)
	fixture.Lock()
	consumers = append(append([]resource.Consumer(nil), fixture.consumers...), consumers...)
	fixture.Unlock()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Consumers: basicAuthLookup(consumers...)})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerAcceptsBasicAuthAndAttachesConsumer(t *testing.T) {
	p := newTestPlugin(t, Config{}, basicAuthBoundConsumer("basic-user", "basic-user", "secret"))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", basicHeader("basic-user", "secret"))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "basic-user" {
			t.Fatalf("consumer_name = %v, want basic-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerUsesInjectedConsumerLookupAuthoritatively(t *testing.T) {
	lookup := &basicAuthConsumerLookup{byKey: map[string]resource.Consumer{
		"basic-lookup-key": basicAuthBoundConsumer("basic-lookup-key", "lookup-basic-user", "lookup-password"),
	}}
	p := newLookupTestPlugin(t, Config{}, lookup)

	t.Run("resolved lookup consumer is authoritative", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
		request = ctx.WithApisixVars(request, map[string]string{})
		request.Header.Set("Authorization", basicHeader("basic-lookup-key", "lookup-password"))
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != "lookup-basic-user" {
				t.Fatalf("consumer_name = %v, want lookup-basic-user", got)
			}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("response code = %d, want 204; body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("lookup miss fails closed", func(t *testing.T) {
		miss := newLookupTestPlugin(t, Config{}, &basicAuthConsumerLookup{})
		request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
		request.Header.Set("Authorization", basicHeader("basic-lookup-key", "lookup-password"))
		response := httptest.NewRecorder()
		miss.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("lookup miss reached downstream")
		})).ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("response code = %d, want 401", response.Code)
		}
	})

	lookup.mu.RLock()
	defer lookup.mu.RUnlock()
	if len(lookup.keyCall) != 1 || lookup.keyCall[0] != name+"\x00basic-lookup-key" {
		t.Fatalf("lookup calls = %#v, want exact factory/key", lookup.keyCall)
	}
}

func TestHandlerUsesInjectedAnonymousConsumerByID(t *testing.T) {
	lookup := &basicAuthConsumerLookup{byID: map[string]resource.Consumer{
		"basic-lookup-anonymous": {Username: "lookup-basic-anonymous", Plugins: map[string]resource.PluginConfig{}},
	}}
	p := newLookupTestPlugin(t, Config{AnonymousConsumer: "basic-lookup-anonymous"}, lookup)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "lookup-basic-anonymous" {
			t.Fatalf("consumer_name = %v, want lookup-basic-anonymous", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", response.Code)
	}
	lookup.mu.RLock()
	defer lookup.mu.RUnlock()
	if len(lookup.idCall) != 1 || lookup.idCall[0] != "basic-lookup-anonymous" {
		t.Fatalf("anonymous lookup calls = %#v", lookup.idCall)
	}

	miss := newLookupTestPlugin(
		t, Config{AnonymousConsumer: "basic-lookup-anonymous"}, &basicAuthConsumerLookup{},
	)
	missResponse := httptest.NewRecorder()
	miss.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("anonymous lookup miss reached downstream")
	})).ServeHTTP(missResponse, httptest.NewRequest(http.MethodGet, "http://example.com/get", nil))
	if missResponse.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous miss response code = %d, want 401", missResponse.Code)
	}
}

func TestBasicAuthConsumerLookupsAreGenerationIsolated(t *testing.T) {
	firstLookup := &basicAuthConsumerLookup{byKey: map[string]resource.Consumer{
		"overlap": basicAuthBoundConsumer("overlap", "basic-generation-n", "password-n"),
	}}
	secondLookup := &basicAuthConsumerLookup{byKey: map[string]resource.Consumer{
		"overlap": basicAuthBoundConsumer("overlap", "basic-generation-n-plus-one", "password-n-plus-one"),
	}}
	first := newLookupTestPlugin(t, Config{}, firstLookup)
	second := newLookupTestPlugin(t, Config{}, secondLookup)

	assertConsumer := func(p *Plugin, password, want string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
		request = ctx.WithApisixVars(request, map[string]string{})
		request.Header.Set("Authorization", basicHeader("overlap", password))
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != want {
				t.Errorf("consumer_name = %v, want %s", got, want)
			}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Errorf("response code = %d, want 204; body=%s", response.Code, response.Body.String())
		}
	}

	var group sync.WaitGroup
	for range 16 {
		group.Go(func() { assertConsumer(first, "password-n", "basic-generation-n") })
		group.Go(func() {
			assertConsumer(second, "password-n-plus-one", "basic-generation-n-plus-one")
		})
	}
	group.Wait()
	firstLookup.close()
	assertConsumer(second, "password-n-plus-one", "basic-generation-n-plus-one")
}

func TestBasicAuthorizationErrorsDoNotExposeCredentials(t *testing.T) {
	for _, header := range []string{"Basic not-base64", "Basic " + base64.StdEncoding.EncodeToString([]byte("alicesecret"))} {
		_, _, err := parseBasicAuthorization(header)
		if err == nil {
			t.Fatalf("parseBasicAuthorization(%q) error = nil", header)
		}
		for _, secret := range []string{"not-base64", "alice", "secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q exposes %q", err, secret)
			}
		}
	}
}

func TestHandlerRecordsProbeDiagnosticInsteadOfDiscardingDetail(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	var diagnostics []string
	req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	req.Header.Set("Authorization", "Basic invalid%%")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid basic authorization reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if len(diagnostics) != 1 || diagnostics[0] != errInvalidBasicEncoding.Error() {
		t.Fatalf("probe diagnostics = %v, want redacted decode failure", diagnostics)
	}
}

func TestHandlerRecordsMissingConsumerProbeDiagnostic(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	var diagnostics []string
	req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	req.Header.Set("Authorization", basicHeader("missing-basic-probe-user", "ignored"))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("missing basic-auth consumer reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if len(diagnostics) != 1 || diagnostics[0] != "failed to find user: invalid user" {
		t.Fatalf("probe diagnostics = %v, want missing-user detail", diagnostics)
	}
}

func TestHandlerNormalizesWhitespaceInCredentials(t *testing.T) {
	consumers := []resource.Consumer{
		basicAuthBoundConsumer("username", "username", "secret"),
		basicAuthBoundConsumer("empty-password-user", "empty-password-user", ""),
	}

	tests := []struct {
		name         string
		username     string
		password     string
		wantCode     int
		wantConsumer string
	}{
		{
			name:         "internal spaces normalized",
			username:     "user name",
			password:     "sec ret",
			wantCode:     http.StatusNoContent,
			wantConsumer: "username",
		},
		{
			name:         "leading username space",
			username:     " username",
			password:     "secret",
			wantCode:     http.StatusNoContent,
			wantConsumer: "username",
		},
		{
			name:         "trailing username space",
			username:     "username ",
			password:     "secret",
			wantCode:     http.StatusNoContent,
			wantConsumer: "username",
		},
		{
			name:         "leading password space",
			username:     "username",
			password:     " secret",
			wantCode:     http.StatusNoContent,
			wantConsumer: "username",
		},
		{
			name:         "trailing password space",
			username:     "username",
			password:     "secret ",
			wantCode:     http.StatusNoContent,
			wantConsumer: "username",
		},
		{
			name:         "empty password",
			username:     "empty-password-user",
			password:     "",
			wantCode:     http.StatusNoContent,
			wantConsumer: "empty-password-user",
		},
		{
			name:     "same-length wrong password",
			username: "username",
			password: "sec rex",
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "different-length wrong password",
			username: "username",
			password: "sec",
			wantCode: http.StatusUnauthorized,
		},
		{
			name:         "spaceful username is normalized to username",
			username:     "user name",
			password:     "secret",
			wantCode:     http.StatusNoContent,
			wantConsumer: "username",
		},
		{
			name:     "non-ASCII NBSP is preserved",
			username: "user\u00a0name",
			password: "secret",
			wantCode: http.StatusUnauthorized,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
			request = ctx.WithApisixVars(request, map[string]string{})
			request.Header.Set("Authorization", basicHeader(test.username, test.password))
			response := httptest.NewRecorder()
			nextCalled := false

			p := newTestPlugin(t, Config{}, consumers...)
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				if test.wantConsumer == "" {
					t.Fatal("unexpected downstream request")
				}
				if got := ctx.GetApisixVar(r, "$consumer_name"); got != test.wantConsumer {
					t.Fatalf("consumer_name = %v, want %q", got, test.wantConsumer)
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, request)

			if response.Code != test.wantCode {
				t.Fatalf("response code = %d, want %d; body=%s", response.Code, test.wantCode, response.Body.String())
			}
			if nextCalled != (test.wantConsumer != "") {
				t.Fatalf("nextCalled = %v, want %v", nextCalled, test.wantConsumer != "")
			}
		})
	}
}

func TestHandlerRejectsMissingAuthorization(t *testing.T) {
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
	if got := rr.Header().Get("WWW-Authenticate"); got != `Basic realm="basic"` {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, `Basic realm="basic"`)
	}
	if !strings.Contains(rr.Body.String(), "Missing authorization in request") {
		t.Fatalf("body = %q, want missing authorization message", rr.Body.String())
	}
}

func TestHandlerFormatsMissingAuthorizationLikeAPISIX(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if got := rr.Body.String(); got != `{"message":"Missing authorization in request"}` {
		t.Fatalf("body = %q, want APISIX response JSON", got)
	}
}

func TestHandlerUsesAnonymousConsumerOnMissingAuthorization(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{AnonymousConsumer: "anonymous-basic-user"},
		basicAuthBoundConsumer("anonymous-basic-user", "anonymous-basic-user", "unused"),
	)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "anonymous-basic-user" {
			t.Fatalf("consumer_name = %v, want anonymous-basic-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestHandlerUsesConfiguredRealm(t *testing.T) {
	p := newTestPlugin(t, Config{Realm: "secure-zone"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("WWW-Authenticate"); got != `Basic realm="secure-zone"` {
		t.Fatalf("WWW-Authenticate = %q, want configured realm", got)
	}
}

func TestHandlerRejectsMalformedAuthorization(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if strings.Contains(rr.Body.String(), "Missing authorization in request") {
		t.Fatalf("body = %q, want malformed authorization message", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Invalid authorization in request") {
		t.Fatalf("body = %q, want invalid authorization message", rr.Body.String())
	}
}

func TestHandlerHideCredentialsRemovesAuthorizationHeader(t *testing.T) {
	hideCredentials := true
	p := newTestPlugin(
		t,
		Config{HideCredentials: &hideCredentials},
		basicAuthBoundConsumer("hide-basic-user", "hide-basic-user", "secret"),
	)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", basicHeader("hide-basic-user", "secret"))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization header = %q, want removed", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerHideCredentialsRemovesDuplicateAuthorizationHeaders(t *testing.T) {
	hideCredentials := true
	p := newTestPlugin(t, Config{
		HideCredentials:   &hideCredentials,
		AnonymousConsumer: "hide-duplicate-anonymous",
	}, basicAuthBoundConsumer("hide-duplicate-anonymous", "hide-duplicate-anonymous", "unused"))

	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Add("Authorization", "")
	request.Header.Add("Authorization", "Bearer attacker")
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := len(r.Header.Values("Authorization")); got != 0 {
			t.Fatalf("Authorization header values = %d, want 0", got)
		}
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "hide-duplicate-anonymous" {
			t.Fatalf("consumer_name = %v, want hide-duplicate-anonymous", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
	if got := len(request.Header.Values("Authorization")); got != 0 {
		t.Fatalf("request Authorization header values = %d, want 0", got)
	}
}

func TestHandlerHideCredentialsOnAnonymousFallback(t *testing.T) {
	invalidConfig := basicAuthBoundConsumer(
		"hide-fallback-invalid-config",
		"hide-fallback-invalid-config",
		"unused",
	)
	invalidConfig.Plugins[name] = map[string]any{
		"username": "hide-fallback-invalid-config",
		"password": []string{"invalid"},
	}
	consumers := []resource.Consumer{
		basicAuthBoundConsumer("hide-fallback-wrong-password", "hide-fallback-wrong-password", "secret"),
		invalidConfig,
		basicAuthBoundConsumer("hide-fallback-anonymous", "hide-fallback-anonymous", "unused"),
	}
	hideCredentials := true

	tests := []struct {
		name              string
		header            string
		username          string
		password          string
		anonymousConsumer string
		wantCode          int
		wantConsumer      string
	}{
		{
			name:              "unknown user",
			username:          "hide-fallback-unknown-user",
			password:          "secret",
			anonymousConsumer: "hide-fallback-anonymous",
			wantCode:          http.StatusNoContent,
			wantConsumer:      "hide-fallback-anonymous",
		},
		{
			name:              "wrong password",
			username:          "hide-fallback-wrong-password",
			password:          "wrong",
			anonymousConsumer: "hide-fallback-anonymous",
			wantCode:          http.StatusNoContent,
			wantConsumer:      "hide-fallback-anonymous",
		},
		{
			name:              "invalid consumer config",
			username:          "hide-fallback-invalid-config",
			password:          "secret",
			anonymousConsumer: "hide-fallback-anonymous",
			wantCode:          http.StatusNoContent,
			wantConsumer:      "hide-fallback-anonymous",
		},
		{
			name:              "anonymous consumer missing",
			username:          "hide-fallback-unknown-anonymous",
			password:          "secret",
			anonymousConsumer: "hide-fallback-missing-anonymous",
			wantCode:          http.StatusUnauthorized,
		},
		{
			name:              "bearer anonymous fallback",
			header:            "Bearer attacker",
			anonymousConsumer: "hide-fallback-anonymous",
			wantCode:          http.StatusNoContent,
			wantConsumer:      "hide-fallback-anonymous",
		},
		{
			name:              "invalid Basic Base64 anonymous fallback",
			header:            "Basic not-base64",
			anonymousConsumer: "hide-fallback-anonymous",
			wantCode:          http.StatusNoContent,
			wantConsumer:      "hide-fallback-anonymous",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
			request = ctx.WithApisixVars(request, map[string]string{})
			header := test.header
			if header == "" {
				header = basicHeader(test.username, test.password)
			}
			request.Header.Set("Authorization", header)
			response := httptest.NewRecorder()
			nextCalled := false

			p := newTestPlugin(t, Config{
				HideCredentials:   &hideCredentials,
				AnonymousConsumer: test.anonymousConsumer,
			}, consumers...)
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				if got := r.Header.Get("Authorization"); got != "" {
					t.Fatalf("Authorization = %q, want removed", got)
				}
				if got := ctx.GetApisixVar(r, "$consumer_name"); got != test.wantConsumer {
					t.Fatalf("consumer_name = %v, want %q", got, test.wantConsumer)
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, request)

			if response.Code != test.wantCode {
				t.Fatalf("response code = %d, want %d; body=%s", response.Code, test.wantCode, response.Body.String())
			}
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("request Authorization = %q, want removed", got)
			}
			if nextCalled != (test.wantConsumer != "") {
				t.Fatalf("nextCalled = %v, want %v", nextCalled, test.wantConsumer != "")
			}
		})
	}
}

func TestParseBasicAuthorizationDiagnostics(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		wantUser string
		wantPass string
		wantErr  string
	}{
		{name: "invalid scheme", header: "Bad_header YmFyOmJhcgo=", wantErr: "Invalid authorization header format"},
		{name: "invalid base64", header: "Basic aca_a", wantErr: "invalid Basic authorization encoding"},
		{name: "missing password", header: "Basic YmFy", wantErr: "invalid Basic authorization value"},
		{name: "case insensitive", header: "bASiC Zm9vOmJhcg==", wantUser: "foo", wantPass: "bar"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user, pass, err := parseBasicAuthorization(test.header)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("parseBasicAuthorization() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBasicAuthorization() error = %v", err)
			}
			if user != test.wantUser || pass != test.wantPass {
				t.Fatalf("credentials = %q/%q, want %q/%q", user, pass, test.wantUser, test.wantPass)
			}
		})
	}
}

func basicHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}
