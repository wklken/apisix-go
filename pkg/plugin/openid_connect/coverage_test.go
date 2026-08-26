package openid_connect

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSessionStorageTTL(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		session sessionData
		cfg     SessionConfig
		want    time.Duration
	}{
		{name: "no timeouts", cfg: SessionConfig{}},
		{
			name:    "absolute only",
			session: sessionData{CreatedAt: now.Add(-time.Minute).Unix()},
			cfg:     SessionConfig{AbsoluteTimeout: 120},
			want:    59 * time.Second,
		},
		{
			name: "rolling only",
			cfg:  SessionConfig{RollingTimeout: 60},
			want: 60 * time.Second,
		},
		{
			name: "idling only",
			cfg:  SessionConfig{IdlingTimeout: 30},
			want: 30 * time.Second,
		},
		{
			name:    "earliest timeout wins",
			session: sessionData{CreatedAt: now.Add(-time.Minute).Unix()},
			cfg: SessionConfig{
				AbsoluteTimeout: 120,
				RollingTimeout:  60,
				IdlingTimeout:   30,
			},
			want: 30 * time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := &Plugin{config: Config{Session: test.cfg}}
			got := plugin.sessionStorageTTL(test.session)
			if test.want == 0 {
				if got != 0 {
					t.Fatalf("sessionStorageTTL() = %s, want no expiry", got)
				}
				return
			}
			if got <= 0 || got < test.want-2*time.Second || got > test.want+2*time.Second {
				t.Fatalf("sessionStorageTTL() = %s, want within 2s of %s", got, test.want)
			}
		})
	}
}

func TestValidClientAuthMethod(t *testing.T) {
	for _, method := range []string{"client_secret_basic", "client_secret_post", "private_key_jwt", "client_secret_jwt"} {
		if !validClientAuthMethod(method) {
			t.Fatalf("validClientAuthMethod(%q) = false, want true", method)
		}
	}
	if validClientAuthMethod("unsupported") {
		t.Fatal("validClientAuthMethod(unsupported) = true, want false")
	}
}

func TestParseRSAPrivateKey(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	pkcs1DER := x509.MarshalPKCS1PrivateKey(privateKey)
	pkcs1PEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: pkcs1DER})
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	pkcs8PEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})

	for name, bytes := range map[string][]byte{
		"pkcs1 pem": pkcs1PEM,
		"pkcs1 der": pkcs1DER,
		"pkcs8 pem": pkcs8PEM,
		"pkcs8 der": pkcs8DER,
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := parseRSAPrivateKey(bytes)
			if err != nil {
				t.Fatalf("parseRSAPrivateKey() error = %v", err)
			}
			if parsed.N.Cmp(privateKey.N) != 0 {
				t.Fatal("parsed key modulus does not match the generated key")
			}
		})
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey(ecdsa) error = %v", err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey(ecdsa) error = %v", err)
	}
	if _, err := parseRSAPrivateKey(ecDER); err == nil {
		t.Fatal("parseRSAPrivateKey(ecdsa) error = nil, want non-RSA rejection")
	}
	if _, err := parseRSAPrivateKey([]byte("not a key")); err == nil {
		t.Fatal("parseRSAPrivateKey(garbage) error = nil")
	}
}

func TestRequestTokensFailsClosedOnInvalidEndpointResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantError  string
	}{
		{name: "HTTP error", statusCode: http.StatusBadGateway, wantError: "token endpoint returned 502"},
		{name: "invalid JSON", statusCode: http.StatusOK, body: "not-json", wantError: "invalid token response"},
		{
			name:       "missing access token",
			statusCode: http.StatusOK,
			body:       `{"refresh_token":"refresh-a"}`,
			wantError:  "token response has no access_token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)

			plugin := &Plugin{
				config: Config{ClientID: "apisix", ClientSecret: "secret-a"},
				client: server.Client(),
				discovery: discoveryData{
					TokenEndpoint: server.URL,
				},
				discoveryLoaded: true,
			}
			if err := plugin.Init(); err != nil {
				t.Fatal(err)
			}
			materializeOIDCTestPlugin(t, plugin)
			req := httptest.NewRequest(http.MethodPost, "https://example.com/callback", nil)
			_, err := plugin.requestTokens(req, url.Values{"grant_type": {"authorization_code"}})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("requestTokens() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestReadRedisSessionFailsClosedOnInvalidState(t *testing.T) {
	newPlugin := func(t *testing.T) *Plugin {
		t.Helper()
		plugin := &Plugin{config: Config{Session: SessionConfig{
			Secret:     "0123456789abcdef",
			CookieName: "oidc_session",
			Storage:    "redis",
			Redis:      &SessionRedisConfig{Prefix: "oidc-sessions"},
		}}}
		if err := plugin.Init(); err != nil {
			t.Fatal(err)
		}
		materializeOIDCTestPlugin(t, plugin)
		return plugin
	}
	requestWithCookie := func(t *testing.T, plugin *Plugin, payload []byte) *http.Request {
		t.Helper()
		value, err := plugin.sealSession(payload)
		if err != nil {
			t.Fatalf("sealSession() error = %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
		req.AddCookie(&http.Cookie{Name: plugin.config.Session.CookieName, Value: value})
		return req
	}

	t.Run("store not configured", func(t *testing.T) {
		plugin := newPlugin(t)
		_, err := plugin.readSession(requestWithCookie(t, plugin, []byte("session-a")))
		if err == nil || !strings.Contains(err.Error(), "Redis session store is not configured") {
			t.Fatalf("readSession() error = %v, want missing store error", err)
		}
	})

	t.Run("empty Redis ID", func(t *testing.T) {
		plugin := newPlugin(t)
		plugin.sessionStore = &fakeSessionStore{values: make(map[string]string)}
		_, err := plugin.readSession(requestWithCookie(t, plugin, nil))
		if err != errSessionNotFound {
			t.Fatalf("readSession() error = %v, want %v", err, errSessionNotFound)
		}
	})

	t.Run("missing backend record", func(t *testing.T) {
		plugin := newPlugin(t)
		plugin.sessionStore = &fakeSessionStore{values: make(map[string]string)}
		_, err := plugin.readSession(requestWithCookie(t, plugin, []byte("session-a")))
		if err != errSessionNotFound {
			t.Fatalf("readSession() error = %v, want %v", err, errSessionNotFound)
		}
	})

	t.Run("invalid backend ciphertext", func(t *testing.T) {
		plugin := newPlugin(t)
		plugin.sessionStore = &fakeSessionStore{values: map[string]string{
			plugin.redisSessionKey("session-a"): "not-ciphertext",
		}}
		if _, err := plugin.readSession(requestWithCookie(t, plugin, []byte("session-a"))); err == nil {
			t.Fatal("readSession() error = nil, want invalid ciphertext rejection")
		}
	})

	t.Run("invalid backend JSON", func(t *testing.T) {
		plugin := newPlugin(t)
		value, err := plugin.sealSession([]byte("not-json"))
		if err != nil {
			t.Fatalf("sealSession() error = %v", err)
		}
		plugin.sessionStore = &fakeSessionStore{values: map[string]string{
			plugin.redisSessionKey("session-a"): value,
		}}
		if _, err := plugin.readSession(requestWithCookie(t, plugin, []byte("session-a"))); err == nil {
			t.Fatal("readSession() error = nil, want invalid JSON rejection")
		}
	})
}

func TestSessionRefreshableHonorsExpiryAndSessionTimeouts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name    string
		config  Config
		session sessionData
		want    bool
	}{
		{name: "missing refresh token", session: sessionData{ExpiresAt: now.Unix()}},
		{name: "missing expiry", session: sessionData{RefreshToken: "refresh-a"}},
		{
			name:   "not near expiry",
			config: Config{AccessTokenExpiresLeeway: 10},
			session: sessionData{
				RefreshToken: "refresh-a",
				ExpiresAt:    now.Add(11 * time.Second).Unix(),
			},
		},
		{
			name:   "absolute timeout reached",
			config: Config{Session: SessionConfig{AbsoluteTimeout: 60}},
			session: sessionData{
				RefreshToken: "refresh-a",
				ExpiresAt:    now.Unix(),
				CreatedAt:    now.Add(-time.Minute).Unix(),
			},
		},
		{
			name:   "idle timeout reached",
			config: Config{Session: SessionConfig{IdlingTimeout: 60}},
			session: sessionData{
				RefreshToken: "refresh-a",
				ExpiresAt:    now.Unix(),
				UpdatedAt:    now.Add(-time.Minute).Unix(),
			},
		},
		{
			name: "refreshable",
			config: Config{
				AccessTokenExpiresLeeway: 10,
				Session:                  SessionConfig{AbsoluteTimeout: 120, IdlingTimeout: 120},
			},
			session: sessionData{
				RefreshToken: "refresh-a",
				ExpiresAt:    now.Add(10 * time.Second).Unix(),
				CreatedAt:    now.Add(-time.Minute).Unix(),
				UpdatedAt:    now.Add(-time.Minute).Unix(),
			},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := &Plugin{config: test.config}
			if got := plugin.sessionRefreshable(test.session, now); got != test.want {
				t.Fatalf("sessionRefreshable() = %t, want %t", got, test.want)
			}
		})
	}
}
