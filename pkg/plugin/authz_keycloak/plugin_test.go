package authz_keycloak

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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

func TestHandlerPostsUMADecisionWithStaticPermissions(t *testing.T) {
	forms := make(chan url.Values, 1)
	authHeaders := make(chan string, 1)
	keycloak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Fatalf("path = %q, want /token", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		forms <- r.PostForm
		authHeaders <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":true}`))
	}))
	t.Cleanup(keycloak.Close)

	p := newTestPlugin(t, Config{
		TokenEndpoint:     keycloak.URL + "/token",
		ClientID:          "apisix",
		Permissions:       []string{"orders"},
		HTTPMethodAsScope: true,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders/1", nil)
	req.Header.Set("Authorization", "raw-token")
	rr := httptest.NewRecorder()
	called := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}

	form := <-forms
	if form.Get("grant_type") != defaultGrantType {
		t.Fatalf("grant_type = %q, want UMA grant", form.Get("grant_type"))
	}
	if form.Get("audience") != "apisix" {
		t.Fatalf("audience = %q, want client id", form.Get("audience"))
	}
	if form.Get("response_mode") != "decision" {
		t.Fatalf("response_mode = %q, want decision", form.Get("response_mode"))
	}
	if got := form["permission"]; len(got) != 1 || got[0] != "orders#POST" {
		t.Fatalf("permission = %v, want [orders#POST]", got)
	}
	if got := <-authHeaders; got != "Bearer raw-token" {
		t.Fatalf("Authorization = %q, want Bearer-prefixed raw token", got)
	}
}

func TestHandlerEnforcingEmptyPermissionsReturnsKeycloakAccessDenied(t *testing.T) {
	p := newTestPlugin(t, Config{
		TokenEndpoint: "http://keycloak.example.com/token",
		ClientID:      "apisix",
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req.Header.Set("Authorization", "Bearer jwt")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if strings.TrimSpace(rr.Body.String()) != `{"error":"access_denied","error_description":"not_authorized"}` {
		t.Fatalf("body = %q, want Keycloak access_denied body", rr.Body.String())
	}
}

func TestHandlerRedirectsWhenAccessDeniedRedirectURIConfigured(t *testing.T) {
	p := newTestPlugin(t, Config{
		TokenEndpoint:           "http://keycloak.example.com/token",
		ClientID:                "apisix",
		AccessDeniedRedirectURI: "/login",
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req.Header.Set("Authorization", "Bearer jwt")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", rr.Code)
	}
	if rr.Header().Get("Location") != "/login" {
		t.Fatalf("Location = %q, want /login", rr.Header().Get("Location"))
	}
}

func TestPasswordGrantEndpointProxiesTokenResponse(t *testing.T) {
	forms := make(chan url.Values, 1)
	keycloak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		forms <- r.PostForm
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"access_token":"token-a"}`))
	}))
	t.Cleanup(keycloak.Close)

	p := newTestPlugin(t, Config{
		TokenEndpoint:                           keycloak.URL + "/token",
		ClientID:                                "apisix",
		ClientSecret:                            "secret-a",
		PasswordGrantTokenGenerationIncomingURI: "/oauth/token",
	})

	body := strings.NewReader("username=alice&password=secret")
	req := httptest.NewRequest(http.MethodPost, "http://example.com/oauth/token", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want token endpoint status", rr.Code)
	}
	if strings.TrimSpace(rr.Body.String()) != `{"access_token":"token-a"}` {
		t.Fatalf("body = %q, want token endpoint body", rr.Body.String())
	}
	form := <-forms
	if form.Get("grant_type") != "password" {
		t.Fatalf("grant_type = %q, want password", form.Get("grant_type"))
	}
	if form.Get("client_id") != "apisix" || form.Get("client_secret") != "secret-a" {
		t.Fatalf("client credentials = %q/%q", form.Get("client_id"), form.Get("client_secret"))
	}
	if form.Get("username") != "alice" || form.Get("password") != "secret" {
		t.Fatalf("user credentials = %q/%q", form.Get("username"), form.Get("password"))
	}
}

func TestLazyLoadDiscoversEndpointsAndResolvesResourcePermissions(t *testing.T) {
	var serviceAccountRequested bool
	var resourceRequested bool
	umaForm := make(chan url.Values, 1)
	keycloak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"token_endpoint":                 "http://" + r.Host + "/token",
				"resource_registration_endpoint": "http://" + r.Host + "/resources",
			})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm() error = %v", err)
			}
			switch r.PostForm.Get("grant_type") {
			case "client_credentials":
				serviceAccountRequested = true
				json.NewEncoder(w).Encode(map[string]any{"access_token": "sa-token", "expires_in": 300})
			case defaultGrantType:
				umaForm <- r.PostForm
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected grant_type %q", r.PostForm.Get("grant_type"))
			}
		case "/resources":
			resourceRequested = true
			if got := r.Header.Get("Authorization"); got != "Bearer sa-token" {
				t.Fatalf("resource Authorization = %q, want service account token", got)
			}
			if r.URL.Query().Get("uri") != "/orders/1" {
				t.Fatalf("resource uri = %q, want request path", r.URL.Query().Get("uri"))
			}
			if r.URL.Query().Get("matchingUri") != "true" {
				t.Fatalf("matchingUri = %q, want true", r.URL.Query().Get("matchingUri"))
			}
			json.NewEncoder(w).Encode([]string{"orders"})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(keycloak.Close)

	p := newTestPlugin(t, Config{
		Discovery:     keycloak.URL + "/.well-known/openid-configuration",
		ClientID:      "apisix",
		ClientSecret:  "secret-a",
		LazyLoadPaths: true,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req.Header.Set("Authorization", "Bearer jwt")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if !serviceAccountRequested {
		t.Fatal("service account token was not requested")
	}
	if !resourceRequested {
		t.Fatal("resource registration endpoint was not requested")
	}
	form := <-umaForm
	if got := form["permission"]; len(got) != 1 || got[0] != "orders" {
		t.Fatalf("permission = %v, want discovered resource", got)
	}
}

func TestLazyLoadRefreshesExpiredServiceAccountToken(t *testing.T) {
	var grantTypes []string
	keycloak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm() error = %v", err)
			}
			grantType := r.PostForm.Get("grant_type")
			grantTypes = append(grantTypes, grantType)
			switch grantType {
			case "refresh_token":
				if got := r.PostForm.Get("refresh_token"); got != "refresh-a" {
					t.Fatalf("refresh_token = %q, want refresh-a", got)
				}
				w.Write([]byte(
					`{"access_token":"sa-token-b","expires_in":300,"refresh_token":"refresh-b","refresh_expires_in":3600}`,
				))
			case defaultGrantType:
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("grant_type = %q, want refresh_token or UMA", grantType)
			}
		case "/resources":
			if got := r.Header.Get("Authorization"); got != "Bearer sa-token-b" {
				t.Fatalf("resource Authorization = %q, want refreshed token", got)
			}
			w.Write([]byte(`["orders"]`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(keycloak.Close)

	p := newTestPlugin(t, Config{
		TokenEndpoint:                keycloak.URL + "/token",
		ResourceRegistrationEndpoint: keycloak.URL + "/resources",
		ClientID:                     "apisix",
		ClientSecret:                 "secret-a",
		LazyLoadPaths:                true,
	})
	p.serviceAccountToken = tokenCache{
		value:                 "sa-token-a",
		expiresAt:             time.Now().Add(-time.Second),
		refreshToken:          "refresh-a",
		refreshTokenExpiresAt: time.Now().Add(time.Hour),
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req.Header.Set("Authorization", "Bearer jwt")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if len(grantTypes) != 2 || grantTypes[0] != "refresh_token" || grantTypes[1] != defaultGrantType {
		t.Fatalf("grant_types = %v, want [refresh_token %s]", grantTypes, defaultGrantType)
	}
	if p.serviceAccountToken.refreshToken != "refresh-b" {
		t.Fatalf("cached refresh token = %q, want refresh-b", p.serviceAccountToken.refreshToken)
	}
}

func TestCacheServiceAccountTokenKeepsExistingRefreshToken(t *testing.T) {
	p := newTestPlugin(t, Config{
		TokenEndpoint: "http://keycloak.example.com/token",
		ClientID:      "apisix",
	})
	previousExpiry := time.Now().Add(20 * time.Minute).Round(0)

	p.cacheServiceAccountToken("http://keycloak.example.com/token", tokenEndpointResponse{
		AccessToken: "sa-token-b",
		ExpiresIn:   300,
	}, tokenCache{
		refreshToken:          "refresh-a",
		refreshTokenExpiresAt: previousExpiry,
	})

	if p.serviceAccountToken.refreshToken != "refresh-a" {
		t.Fatalf("cached refresh token = %q, want refresh-a", p.serviceAccountToken.refreshToken)
	}
	if !p.serviceAccountToken.refreshTokenExpiresAt.Equal(previousExpiry) {
		t.Fatalf(
			"cached refresh expiration = %v, want %v",
			p.serviceAccountToken.refreshTokenExpiresAt,
			previousExpiry,
		)
	}
}

func TestServiceAccountTokenIsSharedByEndpointAndClientID(t *testing.T) {
	var clientCredentialsRequests int
	keycloak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if grantType := r.PostForm.Get("grant_type"); grantType != "client_credentials" {
			t.Fatalf("grant_type = %q, want client_credentials", grantType)
		}
		clientCredentialsRequests++
		w.Write([]byte(`{"access_token":"sa-token","expires_in":300}`))
	}))
	t.Cleanup(keycloak.Close)

	config := Config{
		TokenEndpoint:   keycloak.URL + "/token",
		ClientID:        "apisix",
		ClientSecret:    "secret-a",
		CacheTTLSeconds: 60,
	}
	first := newTestPlugin(t, config)
	second := newTestPlugin(t, config)

	if token, err := first.serviceAccountAccessToken(); err != nil || token != "sa-token" {
		t.Fatalf("first serviceAccountAccessToken() = %q, %v; want sa-token, nil", token, err)
	}
	if token, err := second.serviceAccountAccessToken(); err != nil || token != "sa-token" {
		t.Fatalf("second serviceAccountAccessToken() = %q, %v; want sa-token, nil", token, err)
	}
	if clientCredentialsRequests != 1 {
		t.Fatalf("client credentials requests = %d, want 1", clientCredentialsRequests)
	}
}

func TestDiscoveryIsSharedUntilCacheTTLExpires(t *testing.T) {
	var discoveryRequests int
	keycloak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discoveryRequests++
		json.NewEncoder(w).Encode(map[string]any{
			"token_endpoint":                 "http://" + r.Host + "/token",
			"resource_registration_endpoint": "http://" + r.Host + "/resources",
		})
	}))
	t.Cleanup(keycloak.Close)

	config := Config{
		Discovery:       keycloak.URL + "/.well-known/openid-configuration",
		ClientID:        "apisix",
		CacheTTLSeconds: 60,
	}
	first := newTestPlugin(t, config)
	second := newTestPlugin(t, config)

	if _, err := first.discover(); err != nil {
		t.Fatalf("first discover() error = %v", err)
	}
	if _, err := second.discover(); err != nil {
		t.Fatalf("second discover() error = %v", err)
	}
	if discoveryRequests != 1 {
		t.Fatalf("discovery requests = %d, want 1", discoveryRequests)
	}
}

func TestCacheServiceAccountTokenHonorsCacheTTL(t *testing.T) {
	p := newTestPlugin(t, Config{
		TokenEndpoint:   "http://keycloak.example.com/token",
		ClientID:        "apisix",
		CacheTTLSeconds: 1,
	})

	p.cacheServiceAccountToken("http://keycloak.example.com/token", tokenEndpointResponse{
		AccessToken: "sa-token",
		ExpiresIn:   300,
	}, tokenCache{})

	if p.serviceAccountToken.cacheExpiresAt.Before(time.Now()) {
		t.Fatalf("cache expiration = %v, want future time", p.serviceAccountToken.cacheExpiresAt)
	}
	if p.serviceAccountToken.cacheExpiresAt.After(time.Now().Add(2 * time.Second)) {
		t.Fatalf("cache expiration = %v, want cache TTL close to 1 second", p.serviceAccountToken.cacheExpiresAt)
	}
}

func TestPostInitAppliesKeepaliveAndTLSOptions(t *testing.T) {
	keepalive := false
	sslVerify := false
	p := newTestPlugin(t, Config{
		TokenEndpoint:        "https://keycloak.example.com/token",
		ClientID:             "apisix",
		SSLVerify:            &sslVerify,
		Keepalive:            &keepalive,
		KeepaliveTimeout:     1500,
		KeepalivePool:        7,
		CacheTTLSeconds:      60,
		ClientSecret:         "secret-a",
		AccessTokenExpiresIn: 300,
	})

	transport, ok := p.client.GetClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", p.client.GetClient().Transport)
	}
	if !transport.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = false, want true")
	}
	if transport.IdleConnTimeout != 1500*time.Millisecond {
		t.Fatalf("IdleConnTimeout = %s, want 1.5s", transport.IdleConnTimeout)
	}
	if transport.MaxIdleConnsPerHost != 7 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 7", transport.MaxIdleConnsPerHost)
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("TLSClientConfig = %#v, want InsecureSkipVerify", transport.TLSClientConfig)
	}
}
