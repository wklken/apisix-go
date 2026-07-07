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
)

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s := store.NewStore(t.TempDir()+"/ldap-auth.db", testEvents)
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

func ldapRequest(username, password string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	req.Header.Set("Authorization", "Basic "+token)
	return req
}
