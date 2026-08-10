package ldap_auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s, err := store.GetStore(t.TempDir()+"/ldap-auth.db", testEvents)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		s.Start()
	})
}

func addLDAPConsumer(t *testing.T, username, userDN string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"ldap-auth": map[string]any{
				"user_dn": userDN,
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
		if _, err := store.GetConsumerByPluginKey("ldap-auth", userDN); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for ldap-auth user_dn %q", username, userDN)
}

func newTestPlugin(t *testing.T, authenticate ldapAuthenticator) *Plugin {
	t.Helper()

	p := &Plugin{
		config: Config{
			BaseDN:  "dc=example,dc=org",
			LDAPURI: "ldap://127.0.0.1:389",
		},
		authenticate: authenticate,
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
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
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

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
	setupStore(t)
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
