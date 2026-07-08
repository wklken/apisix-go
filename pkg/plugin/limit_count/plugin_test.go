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

func TestHandlerAppliesResolvedRules(t *testing.T) {
	p := newTestPlugin(t, Config{
		RejectedCode: http.StatusTooManyRequests,
		Rules: []Rule{
			{
				Count:        3,
				TimeWindow:   60,
				Key:          "$http_x_tenant",
				HeaderPrefix: "Tenant",
			},
			{
				Count:        1,
				TimeWindow:   60,
				Key:          "$http_x_user",
				HeaderPrefix: "User",
			},
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.Header.Set("X-Tenant", "t1")
	first.Header.Set("X-User", "alice")
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}
	if got := firstRecorder.Header().Get("X-User-RateLimit-Limit"); got != "1" {
		t.Fatalf("user limit header = %q, want 1", got)
	}
	if got := firstRecorder.Header().Get("X-Tenant-RateLimit-Limit"); got != "3" {
		t.Fatalf("tenant limit header = %q, want 3", got)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.Header.Set("X-Tenant", "t1")
	second.Header.Set("X-User", "alice")
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want user rule rejection", secondRecorder.Code)
	}
	if got := secondRecorder.Header().Get("X-User-RateLimit-Remaining"); got != "0" {
		t.Fatalf("user remaining header = %q, want 0", got)
	}

	third := httptest.NewRequest(http.MethodGet, "/", nil)
	third.Header.Set("X-Tenant", "t1")
	third.Header.Set("X-User", "bob")
	third.RemoteAddr = "192.0.2.1:1234"
	thirdRecorder := httptest.NewRecorder()
	handler.ServeHTTP(thirdRecorder, third)
	if thirdRecorder.Code != http.StatusNoContent {
		t.Fatalf("third status = %d, want tenant rule still allows second tenant request", thirdRecorder.Code)
	}
}

func TestHandlerUsesMetadataQuotaHeaderNames(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		RejectedCode: http.StatusTooManyRequests,
	})
	p.metadata = Metadata{
		LimitHeader:     "X-Custom-Limit",
		RemainingHeader: "X-Custom-Remaining",
		ResetHeader:     "X-Custom-Reset",
	}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}
	if got := firstRecorder.Header().Get("X-Custom-Limit"); got != "1" {
		t.Fatalf("custom limit header = %q, want 1", got)
	}
	if got := firstRecorder.Header().Get("X-RateLimit-Limit"); got != "" {
		t.Fatalf("default limit header = %q, want empty", got)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want rejection", secondRecorder.Code)
	}
	if got := secondRecorder.Header().Get("X-Custom-Remaining"); got != "0" {
		t.Fatalf("custom remaining header = %q, want 0", got)
	}
	if got := secondRecorder.Header().Get("X-Custom-Reset"); got == "" {
		t.Fatal("custom reset header is empty, want reset timestamp")
	}
}

func TestPostInitRejectsDuplicateRuleKeys(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Count: 1, TimeWindow: 60, Key: "$http_x_user"},
			{Count: 2, TimeWindow: 60, Key: "$http_x_user"},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want duplicate rule key error")
	}
}
