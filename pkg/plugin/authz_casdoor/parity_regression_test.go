package authz_casdoor

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParityUnusableTokenResponseIsBare503(t *testing.T) {
	for _, body := range []string{`{}`, `{"error":"provider diagnostic"}`, `not-json`} {
		t.Run(body, func(t *testing.T) {
			provider := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }),
			)
			defer provider.Close()
			p := newTestPlugin(
				t,
				Config{
					EndpointAddr: provider.URL,
					ClientID:     "client-a",
					ClientSecret: testClientSecret,
					CallbackURL:  "http://gateway.example.com/callback",
				},
			)
			start := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
				ServeHTTP(start, httptest.NewRequest("GET", "http://gateway.example.com/private", nil))
			cookie := findSessionCookie(start.Result().Cookies())
			if cookie == nil {
				t.Fatal("session missing")
			}
			callback := httptest.NewRequest("GET", "http://gateway.example.com/callback?code=code-a&state=state-1", nil)
			callback.AddCookie(cookie)
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("callback reached upstream") })).
				ServeHTTP(response, callback)
			if response.Code != 503 || response.Body.Len() != 0 || response.Header().Get("Content-Type") != "" {
				t.Fatalf("status=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
			}
		})
	}
}
