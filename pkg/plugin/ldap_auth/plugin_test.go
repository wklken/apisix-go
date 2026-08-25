package ldap_auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type ldapConsumerLookup struct {
	mu       sync.RWMutex
	byKey    map[string]resource.Consumer
	calls    []string
	onLookup func()
	closed   bool
}

func (lookup *ldapConsumerLookup) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.calls = append(lookup.calls, plugin+"\x00"+key)
	if lookup.onLookup != nil {
		lookup.onLookup()
	}
	if lookup.closed {
		return resource.Consumer{}, false
	}
	consumer, ok := lookup.byKey[key]
	return consumer, ok
}

func (*ldapConsumerLookup) ConsumerByID(string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (*ldapConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func (lookup *ldapConsumerLookup) close() {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.closed = true
	lookup.byKey = nil
}

func newLookupTestPlugin(t *testing.T, lookup base.ConsumerLookup, authenticate ldapAuthenticator) *Plugin {
	t.Helper()
	p := &Plugin{
		config:       Config{BaseDN: "dc=example,dc=org", LDAPURI: "ldap://127.0.0.1:389"},
		authenticate: authenticate,
	}
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p
}

type ldapTestFixture struct {
	sync.Mutex
	byKey map[string]resource.Consumer
}

var ldapTestFixtures sync.Map

func ldapFixtureFor(t *testing.T) *ldapTestFixture {
	t.Helper()
	fixture := &ldapTestFixture{byKey: map[string]resource.Consumer{}}
	actual, loaded := ldapTestFixtures.LoadOrStore(t, fixture)
	if !loaded {
		t.Cleanup(func() { ldapTestFixtures.Delete(t) })
	}
	return actual.(*ldapTestFixture)
}

func addLDAPConsumer(t *testing.T, username, userDN string) {
	t.Helper()
	fixture := ldapFixtureFor(t)
	fixture.Lock()
	defer fixture.Unlock()
	fixture.byKey[userDN] = resource.Consumer{
		Username: username,
		Plugins:  map[string]resource.PluginConfig{name: map[string]any{"user_dn": userDN}},
	}
}

func newTestPlugin(t *testing.T, authenticate ldapAuthenticator) *Plugin {
	return newTestPluginWithConfig(t, nil, authenticate)
}

func newTestPluginWithConfig(t *testing.T, overrides map[string]any, authenticate ldapAuthenticator) *Plugin {
	t.Helper()

	config := Config{
		BaseDN:  "dc=example,dc=org",
		LDAPURI: "ldap://127.0.0.1:389",
	}
	if overrides != nil {
		if err := util.Parse(overrides, &config); err != nil {
			t.Fatalf("parse config: %v", err)
		}
	}

	p := &Plugin{
		config:       config,
		authenticate: authenticate,
	}
	fixture := ldapFixtureFor(t)
	fixture.Lock()
	byKey := make(map[string]resource.Consumer, len(fixture.byKey))
	maps.Copy(byKey, fixture.byKey)
	fixture.Unlock()
	p.SetDependencies(base.Dependencies{Consumers: &ldapConsumerLookup{byKey: byKey}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestLDAPSchemaSupportsHideCredentialsAndDefaultsFalse(t *testing.T) {
	p := newTestPlugin(t, nil)
	config := map[string]any{
		"base_dn":          "dc=example,dc=org",
		"ldap_uri":         "ldap://127.0.0.1:389",
		"hide_credentials": true,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("hide_credentials should validate: %v", err)
	}

	config["hide_credentials"] = "true"
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("hide_credentials should reject non-boolean values")
	}

	encoded, err := json.Marshal(p.Config())
	if err != nil {
		t.Fatalf("marshal default config: %v", err)
	}
	if !strings.Contains(string(encoded), `"hide_credentials":false`) {
		t.Fatalf("default config = %s, want hide_credentials=false", encoded)
	}
}

func TestLDAPTLSVerifyDefaultsToEnabledAndSupportsExplicitOptOut(t *testing.T) {
	p := newTestPlugin(t, nil)
	if p.config.TLSVerify == nil || !*p.config.TLSVerify {
		t.Fatalf("TLSVerify = %v, want true when omitted", p.config.TLSVerify)
	}
	verified, err := ldapTLSConfig(p.config)
	if err != nil {
		t.Fatalf("ldapTLSConfig(omitted) error = %v", err)
	}
	if verified.InsecureSkipVerify {
		t.Fatal("ldapTLSConfig(omitted) enabled InsecureSkipVerify")
	}

	optOut := Config{TLSVerify: new(false)}
	insecure, err := ldapTLSConfig(optOut)
	if err != nil {
		t.Fatalf("ldapTLSConfig(explicit false) error = %v", err)
	}
	if !insecure.InsecureSkipVerify {
		t.Fatal("ldapTLSConfig(explicit false) did not enable InsecureSkipVerify")
	}
}

func TestHandlerAuthenticatesLDAPUserAndAttachesConsumer(t *testing.T) {
	addLDAPConsumer(t, "ldap-user", "cn=alice,dc=example,dc=org")
	p := newTestPlugin(t, func(username, password string, cfg Config) error {
		if username != "alice" {
			t.Fatalf("username = %q, want alice", username)
		}
		if password != "secret" {
			t.Fatalf("password = %q, want secret", password)
		}
		return nil
	})

	req := ldapRequest("alice", "secret")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "ldap-user" {
			t.Fatalf("consumer_name = %v, want ldap-user", got)
		}
		consumer, ok := ctx.GetApisixVar(r, "$consumer").(resource.Consumer)
		if !ok {
			t.Fatalf("consumer = %T, want resource.Consumer", ctx.GetApisixVar(r, "$consumer"))
		}
		if config := consumer.Plugins["ldap-auth"]; config != nil {
			t.Fatalf("consumer ldap-auth config = %#v, want redacted", config)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerUsesInjectedConsumerLookupAfterLDAPBind(t *testing.T) {
	const userDN = "cn=lookup-user,dc=example,dc=org"
	var orderMu sync.Mutex
	var order []string
	lookup := &ldapConsumerLookup{
		byKey: map[string]resource.Consumer{userDN: {
			Username: "lookup-ldap-user",
			Plugins:  map[string]resource.PluginConfig{name: map[string]any{"user_dn": userDN}},
		}},
		onLookup: func() {
			orderMu.Lock()
			order = append(order, "lookup")
			orderMu.Unlock()
		},
	}
	p := newLookupTestPlugin(t, lookup, func(username, password string, _ Config) error {
		if username != "lookup-user" || password != "secret" {
			t.Fatalf("bind credentials = %q/%q", username, password)
		}
		orderMu.Lock()
		order = append(order, "bind")
		orderMu.Unlock()
		return nil
	})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "lookup-ldap-user" {
			t.Fatalf("consumer_name = %v, want lookup-ldap-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, ldapRequest("lookup-user", "secret"))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", response.Code, response.Body.String())
	}
	orderMu.Lock()
	if strings.Join(order, ",") != "bind,lookup" {
		t.Fatalf("operation order = %v, want bind then lookup", order)
	}
	orderMu.Unlock()
	lookup.mu.RLock()
	if len(lookup.calls) != 1 || lookup.calls[0] != name+"\x00"+userDN {
		t.Fatalf("lookup calls = %#v, want exact factory/user_dn", lookup.calls)
	}
	lookup.mu.RUnlock()
}

func TestInjectedLDAPLookupMissFailsClosed(t *testing.T) {
	bindCalls := 0
	p := newLookupTestPlugin(t, &ldapConsumerLookup{}, func(username, password string, _ Config) error {
		bindCalls++
		return nil
	})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("LDAP lookup miss reached downstream")
	})).ServeHTTP(response, ldapRequest("ldap-miss", "secret"))
	if bindCalls != 1 {
		t.Fatalf("LDAP bind calls = %d, want one before lookup", bindCalls)
	}
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestLDAPConsumerLookupsAreGenerationIsolated(t *testing.T) {
	const userDN = "cn=overlap,dc=example,dc=org"
	firstLookup := &ldapConsumerLookup{byKey: map[string]resource.Consumer{
		userDN: {Username: "ldap-generation-n", Plugins: map[string]resource.PluginConfig{}},
	}}
	secondLookup := &ldapConsumerLookup{byKey: map[string]resource.Consumer{
		userDN: {Username: "ldap-generation-n-plus-one", Plugins: map[string]resource.PluginConfig{}},
	}}
	first := newLookupTestPlugin(t, firstLookup, func(string, string, Config) error { return nil })
	second := newLookupTestPlugin(t, secondLookup, func(string, string, Config) error { return nil })
	assertConsumer := func(p *Plugin, want string) {
		t.Helper()
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != want {
				t.Errorf("consumer_name = %v, want %s", got, want)
			}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, ldapRequest("overlap", "secret"))
		if response.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", response.Code)
		}
	}
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() { assertConsumer(first, "ldap-generation-n") })
		group.Go(func() { assertConsumer(second, "ldap-generation-n-plus-one") })
	}
	group.Wait()
	firstLookup.close()
	assertConsumer(second, "ldap-generation-n-plus-one")
}

func TestUserDNEscapesRFC4514Metacharacters(t *testing.T) {
	p := newTestPlugin(t, nil)

	if got := p.userDN(`alice,ou=admins`); got != `cn=alice\,ou=admins,dc=example,dc=org` {
		t.Fatalf("userDN() = %q, want escaped RDN value", got)
	}
}

func TestHandlerPreservesAuthorizationHeaderByDefault(t *testing.T) {
	addLDAPConsumer(t, "ldap-default-visible-user", "cn=default-visible,dc=example,dc=org")
	p := newTestPlugin(t, func(username, password string, cfg Config) error {
		return nil
	})
	req := ldapRequest("default-visible", "secret")
	wantAuthorization := req.Header.Get("Authorization")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuthorization {
			t.Fatalf("Authorization header = %q, want preserved value %q", got, wantAuthorization)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerHideCredentialsRemovesAuthorizationHeaderAfterSuccessfulLDAPAuth(t *testing.T) {
	addLDAPConsumer(t, "ldap-hidden-user", "cn=hidden,dc=example,dc=org")
	p := newTestPluginWithConfig(
		t,
		map[string]any{"hide_credentials": true},
		func(username, password string, cfg Config) error {
			return nil
		},
	)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization header = %q, want removed", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, ldapRequest("hidden", "secret"))

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMissingAuthorization(t *testing.T) {
	p := newTestPlugin(t, func(username, password string, cfg Config) error {
		t.Fatal("LDAP authenticator should not be called")
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Basic realm="ldap"` {
		t.Fatalf("WWW-Authenticate = %q, want Basic ldap realm", got)
	}
	if !strings.Contains(rr.Body.String(), "Missing authorization in request") {
		t.Fatalf("body = %q, want missing authorization message", rr.Body.String())
	}
}

func TestHandlerRejectsInvalidAuthorizationHeader(t *testing.T) {
	p := newTestPlugin(t, func(username, password string, cfg Config) error {
		t.Fatal("LDAP authenticator should not be called")
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid authorization in request") {
		t.Fatalf("body = %q, want invalid authorization message", rr.Body.String())
	}
}

func TestHandlerRejectsEmptyOrWhitespacePassword(t *testing.T) {
	for _, password := range []string{"", " "} {
		t.Run(fmt.Sprintf("password=%q", password), func(t *testing.T) {
			p := newTestPlugin(t, func(username, password string, cfg Config) error {
				t.Fatal("LDAP authenticator should not be called")
				return nil
			})
			req := ldapRequest("admin", password)
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler should not be called")
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
		})
	}
}

func TestHandlerRejectsMalformedAuthorizationWithHideCredentials(t *testing.T) {
	p := newTestPluginWithConfig(
		t,
		map[string]any{"hide_credentials": true},
		func(username, password string, cfg Config) error {
			t.Fatal("LDAP authenticator should not be called")
			return nil
		},
	)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("malformed authorization reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestHandlerRecordsAuthorizationDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		diagnostic string
		forbidden  []string
	}{
		{
			name:       "invalid scheme",
			header:     "Bad_header Zm9vOmZvbwo=",
			diagnostic: "Invalid authorization header format",
			forbidden:  []string{"Zm9vOmZvbwo=", "foo:foo"},
		},
		{
			name:       "invalid base64",
			header:     "Basic aca_a",
			diagnostic: "Failed to decode authentication header",
			forbidden:  []string{"aca_a"},
		},
		{
			name:       "missing password",
			header:     "Basic Zm9v",
			diagnostic: "Split authorization err: invalid decoded data",
			forbidden:  []string{"Zm9v", "foo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, func(username, password string, cfg Config) error {
				t.Fatal("LDAP authenticator should not be called")
				return nil
			})
			req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
			var diagnostics []string
			req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
				diagnostics = append(diagnostics, message)
			})
			req.Header.Set("Authorization", tt.header)
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid authorization reached downstream")
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
			if len(diagnostics) != 1 || diagnostics[0] != tt.diagnostic {
				t.Fatalf("diagnostics = %v, want [%q]", diagnostics, tt.diagnostic)
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(diagnostics[0], forbidden) {
					t.Errorf("diagnostic %q contains authorization payload %q", diagnostics[0], forbidden)
				}
			}
		})
	}
}

func TestHandlerRejectsFailedLDAPBind(t *testing.T) {
	addLDAPConsumer(t, "bad-ldap-user", "cn=bob,dc=example,dc=org")
	p := newTestPlugin(t, func(username, password string, cfg Config) error {
		return errors.New("invalid credentials")
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, ldapRequest("bob", "wrong"))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid user authorization") {
		t.Fatalf("body = %q, want invalid user authorization message", rr.Body.String())
	}
}

func TestHandlerRejectsFailedLDAPBindWithHideCredentials(t *testing.T) {
	p := newTestPluginWithConfig(
		t,
		map[string]any{"hide_credentials": true},
		func(username, password string, cfg Config) error {
			return errors.New("invalid credentials")
		},
	)

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("failed LDAP bind reached downstream")
	})).ServeHTTP(rr, ldapRequest("hidden-failure", "wrong"))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestHandlerRecordsLDAPBindFailureDiagnostic(t *testing.T) {
	p := newTestPlugin(t, func(username, password string, cfg Config) error {
		return errors.New("LDAP Result Code 49 \"Invalid Credentials\": The supplied credential is invalid")
	})
	req := ldapRequest("bob", "wrong")
	var diagnostics []string
	req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("failed LDAP bind reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(diagnostics) != 1 ||
		diagnostics[0] != `ldap-auth failed: LDAP Result Code 49 "Invalid Credentials": The supplied credential is invalid` {
		t.Fatalf("diagnostics = %v, want LDAP bind failure detail", diagnostics)
	}
}

func TestHandlerRejectsMissingRelatedConsumer(t *testing.T) {
	p := newTestPlugin(t, func(username, password string, cfg Config) error {
		return nil
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, ldapRequest("missing", "secret"))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid user authorization") {
		t.Fatalf("body = %q, want invalid user authorization message", rr.Body.String())
	}
}

func TestHandlerRecordsMissingConsumerDiagnostic(t *testing.T) {
	p := newTestPlugin(t, func(username, password string, cfg Config) error {
		return nil
	})
	req := ldapRequest("missing-diagnostic", "secret")
	var diagnostics []string
	req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("missing LDAP consumer reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(diagnostics) != 1 || diagnostics[0] != "failed to find user: invalid user" {
		t.Fatalf("diagnostics = %v, want missing-user detail", diagnostics)
	}
}

func TestHandlerWritesExactJSONAuthorizationErrors(t *testing.T) {
	tests := []struct {
		name    string
		request func() *http.Request
		auth    ldapAuthenticator
		body    string
	}{
		{
			name: "missing authorization",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
			},
			auth: func(username, password string, cfg Config) error {
				t.Fatal("LDAP authenticator should not be called")
				return nil
			},
			body: `{"message":"Missing authorization in request"}`,
		},
		{
			name: "invalid authorization",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
				req.Header.Set("Authorization", "Basic not-base64")
				return req
			},
			auth: func(username, password string, cfg Config) error {
				t.Fatal("LDAP authenticator should not be called")
				return nil
			},
			body: `{"message":"Invalid authorization in request"}`,
		},
		{
			name: "invalid user authorization",
			request: func() *http.Request {
				return ldapRequest("alice", "wrong")
			},
			auth: func(username, password string, cfg Config) error {
				return errors.New("invalid credentials")
			},
			body: `{"message":"Invalid user authorization"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, tt.auth)
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("authorization failure reached downstream")
			})).ServeHTTP(rr, tt.request())

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
			if got := rr.Body.String(); got != tt.body {
				t.Fatalf("body = %q, want %q", got, tt.body)
			}
			if got := rr.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

func TestLDAPDialURLUsesLDAPSForHostAddressWhenTLSIsEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "plain host",
			cfg:  Config{LDAPURI: "ldap.example.com:1389"},
			want: "ldap://ldap.example.com:1389",
		},
		{
			name: "TLS host",
			cfg:  Config{LDAPURI: "ldap.example.com:636", UseTLS: true},
			want: "ldaps://ldap.example.com:636",
		},
		{
			name: "explicit LDAP URL honors TLS setting",
			cfg:  Config{LDAPURI: "ldap://ldap.example.com:1389", UseTLS: true},
			want: "ldaps://ldap.example.com:1389",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ldapDialURL(tt.cfg); got != tt.want {
				t.Fatalf("ldapDialURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRealmSchemaMatchesUpstreamChallengeConstraints(t *testing.T) {
	p := newTestPlugin(t, nil)
	valid := []string{
		"ldap",
		"my-ldap-realm",
		strings.Repeat("a", 128),
		" !#$%&'()*+,-./:;<=>?@[]^_`{|}~",
	}
	for _, realm := range valid {
		config := map[string]any{
			"base_dn":  "dc=example,dc=org",
			"ldap_uri": "ldap://127.0.0.1:389",
			"realm":    realm,
		}
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Errorf("realm %q should validate: %v", realm, err)
		}
	}

	invalid := []string{
		"",
		strings.Repeat("a", 129),
		`bad"realm`,
		`bad\realm`,
		"bad\nrealm",
		"bad\x7frealm",
	}
	for _, realm := range invalid {
		config := map[string]any{
			"base_dn":  "dc=example,dc=org",
			"ldap_uri": "ldap://127.0.0.1:389",
			"realm":    realm,
		}
		if err := util.Validate(config, p.GetSchema()); err == nil {
			t.Errorf("realm %q should be rejected", realm)
		}
	}
}

func ldapRequest(username, password string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	req.Header.Set("Authorization", "Basic "+token)
	return req
}
