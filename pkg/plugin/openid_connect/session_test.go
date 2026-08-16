package openid_connect

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionCookieSecureValuesOnWrite(t *testing.T) {
	tests := []struct {
		name       string
		configured *bool
		wantSecure bool
	}{
		{name: "omitted", wantSecure: true},
		{name: "explicit true", configured: new(true), wantSecure: true},
		{name: "explicit false", configured: new(false), wantSecure: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := sessionCookieTestConfig()
			if test.configured != nil {
				config.Session.CookieSecure = test.configured
			}
			plugin := newTestPlugin(t, config)
			writer := httptest.NewRecorder()
			if err := plugin.writeSession(writer, sessionData{
				CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
			}); err != nil {
				t.Fatalf("writeSession() error = %v", err)
			}

			assertSessionCookieAttributes(t, writer.Result().Cookies(), test.wantSecure)
		})
	}
}

func TestSessionCookieSecureValuesOnClear(t *testing.T) {
	tests := []struct {
		name       string
		configured *bool
		wantSecure bool
	}{
		{name: "omitted", wantSecure: true},
		{name: "explicit true", configured: new(true), wantSecure: true},
		{name: "explicit false", configured: new(false), wantSecure: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := sessionCookieTestConfig()
			if test.configured != nil {
				config.Session.CookieSecure = test.configured
			}
			plugin := newTestPlugin(t, config)
			writer := httptest.NewRecorder()
			plugin.clearSession(writer, nil)

			cookies := writer.Result().Cookies()
			assertSessionCookieAttributes(t, cookies, test.wantSecure)
			if cookies[0].MaxAge >= 0 {
				t.Fatalf("clear cookie MaxAge = %d, want negative", cookies[0].MaxAge)
			}
		})
	}
}

func TestBearerOnlyDoesNotSetSessionCookie(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/introspect" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true})
	}))
	t.Cleanup(idp.Close)

	plugin := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		Discovery:             "https://idp.example.com/.well-known/openid-configuration",
		IntrospectionEndpoint: idp.URL + "/introspect",
		BearerOnly:            true,
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	request.Header.Set("Authorization", "Bearer token-a")
	plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", recorder.Code, recorder.Body.String())
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("bearer-only response cookies = %#v, want none", cookies)
	}
}

func sessionCookieTestConfig() Config {
	return Config{
		ClientID:     "apisix",
		ClientSecret: "secret-a",
		Discovery:    "https://idp.example.com/.well-known/openid-configuration",
		Session: SessionConfig{
			Secret:         "0123456789abcdef",
			CookieHTTPOnly: new(true),
			CookieSameSite: "Lax",
		},
	}
}

func assertSessionCookieAttributes(t *testing.T, cookies []*http.Cookie, wantSecure bool) {
	t.Helper()
	if len(cookies) != 1 {
		t.Fatalf("session cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Secure != wantSecure {
		t.Fatalf("session cookie Secure = %t, want %t", cookie.Secure, wantSecure)
	}
	if !cookie.HttpOnly {
		t.Fatal("session cookie HttpOnly = false, want true")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite = %v, want Lax", cookie.SameSite)
	}
}
