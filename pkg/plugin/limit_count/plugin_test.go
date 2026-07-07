package limit_count

import (
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestHandlerUsesHTTPVariableKey(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		Key:          "http_x_user",
		KeyType:      "var",
		RejectedCode: http.StatusTooManyRequests,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.Header.Set("X-User", "alice")
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.Header.Set("X-User", "bob")
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusNoContent {
		t.Fatalf("second status = %d, want separate quota bucket for different X-User", secondRecorder.Code)
	}
}

func TestHandlerUsesVariableCombinationKey(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		Key:          "$http_x_tenant:$http_x_user",
		KeyType:      "var_combination",
		RejectedCode: http.StatusTooManyRequests,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	requests := []struct {
		tenant string
		user   string
	}{
		{tenant: "t1", user: "alice"},
		{tenant: "t1", user: "bob"},
	}
	for _, req := range requests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Tenant", req.tenant)
		r.Header.Set("X-User", req.user)
		r.RemoteAddr = "192.0.2.1:1234"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, r)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status for %s/%s = %d, want separate quota bucket", req.tenant, req.user, rr.Code)
		}
	}
}
