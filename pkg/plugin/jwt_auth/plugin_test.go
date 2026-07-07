package jwt_auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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
		s := store.NewStore(t.TempDir()+"/jwt-auth.db", testEvents)
		s.Start()
	})
}

func addJWTConsumer(t *testing.T, username, key, secret string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"jwt-auth": map[string]any{
				"key":       key,
				"secret":    secret,
				"algorithm": "HS256",
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
		if _, err := store.GetConsumerByPluginKey("jwt-auth", key); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for jwt-auth key %q", username, key)
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

func TestHandlerAcceptsBearerTokenAndAttachesConsumer(t *testing.T) {
	addJWTConsumer(t, "jwt-user", "jwt-key", "jwt-secret")
	p := newTestPlugin(t, Config{})
	token := signHS256(t, "jwt-secret", map[string]any{
		"key": "jwt-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "jwt-user" {
			t.Fatalf("consumer_name = %v, want jwt-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	res := performRequest(handler, token)
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsMissingToken(t *testing.T) {
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
	if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer realm="jwt"` {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, `Bearer realm="jwt"`)
	}
	if !strings.Contains(rr.Body.String(), "Missing JWT token in request") {
		t.Fatalf("body = %q, want missing token message", rr.Body.String())
	}
}

func TestHandlerRejectsInvalidSignature(t *testing.T) {
	addJWTConsumer(t, "bad-signature-user", "bad-signature-key", "jwt-secret")
	p := newTestPlugin(t, Config{})
	token := signHS256(t, "wrong-secret", map[string]any{
		"key": "bad-signature-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})), token)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(res.Body.String(), "failed to verify jwt") {
		t.Fatalf("body = %q, want verification failure message", res.Body.String())
	}
}

func TestHandlerRejectsExpiredTokenByDefault(t *testing.T) {
	addJWTConsumer(t, "expired-user", "expired-key", "jwt-secret")
	p := newTestPlugin(t, Config{})
	token := signHS256(t, "jwt-secret", map[string]any{
		"key": "expired-key",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})), token)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(res.Body.String(), "failed to verify jwt") {
		t.Fatalf("body = %q, want verification failure message", res.Body.String())
	}
}

func TestHandlerHideCredentialsRemovesAuthorizationHeader(t *testing.T) {
	addJWTConsumer(t, "hide-user", "hide-key", "jwt-secret")
	hideCredentials := true
	p := newTestPlugin(t, Config{HideCredentials: &hideCredentials})
	token := signHS256(t, "jwt-secret", map[string]any{
		"key": "hide-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization header = %q, want removed", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	res := performRequest(handler, token)
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func performRequest(handler http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func signHS256(t *testing.T, secret string, payload map[string]any) string {
	t.Helper()

	header := map[string]any{
		"typ": "JWT",
		"alg": "HS256",
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	unsigned := fmt.Sprintf(
		"%s.%s",
		base64.RawURLEncoding.EncodeToString(headerJSON),
		base64.RawURLEncoding.EncodeToString(payloadJSON),
	)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))

	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
