package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminKeyMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		wantStatus int
		wantNext   bool
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "incorrect", key: "wrong", wantStatus: http.StatusUnauthorized},
		{name: "accepted", key: "secret", wantStatus: http.StatusNoContent, wantNext: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			handler := AdminKeyMiddleware("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodGet, "/v1/routes", nil)
			if test.key != "" {
				request.Header.Set("X-API-KEY", test.key)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || called != test.wantNext {
				t.Fatalf("status/called = %d/%t, want %d/%t", response.Code, called, test.wantStatus, test.wantNext)
			}
		})
	}
}
