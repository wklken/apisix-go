package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestAdminKeyMiddlewareConstantTimeComparison(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		wantStatus int
		wantNext   bool
	}{
		{name: "equal", key: "secret", wantStatus: http.StatusNoContent, wantNext: true},
		{name: "unequal same length", key: "secrex", wantStatus: http.StatusUnauthorized},
		{name: "unequal shorter", key: "secre", wantStatus: http.StatusUnauthorized},
		{name: "unequal longer", key: "secret-long", wantStatus: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			handler := AdminKeyMiddleware("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodGet, "/v1/routes", nil)
			request.Header.Set("X-API-KEY", test.key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || called != test.wantNext {
				t.Fatalf("status/called = %d/%t, want %d/%t", response.Code, called, test.wantStatus, test.wantNext)
			}
		})
	}
}

func TestAdminMiddlewareSourceGuardRejectsDirectComparison(t *testing.T) {
	source, err := os.ReadFile("middleware.go")
	if err != nil {
		t.Fatalf("read middleware.go: %v", err)
	}
	if bytes.Contains(source, []byte("apiKey != adminKey")) {
		t.Fatal("middleware.go compares credentials with a direct non-constant-time expression")
	}
}
