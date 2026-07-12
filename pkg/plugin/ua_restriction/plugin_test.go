package ua_restriction

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestSchemaRejectsAllowlistAndDenylistTogether(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"allowlist": []any{"allowed-bot"},
		"denylist":  []any{"blocked-bot"},
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("allowlist and denylist should not validate together")
	}
}

func TestSchemaRejectsMissingAndConflictingUserAgentLists(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, test := range []struct {
		name   string
		config map[string]any
	}{
		{name: "missing lists", config: map[string]any{}},
		{
			name: "both lists",
			config: map[string]any{
				"allowlist": []any{"allowed-bot"},
				"denylist":  []any{"blocked-bot"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(test.config, p.GetSchema()); err == nil {
				t.Fatal("schema validation error = nil, want official oneOf rejection")
			}
		})
	}
}

func TestDenylistRejectsWithJSONMessage(t *testing.T) {
	p := newTestPlugin(t, Config{DenyList: []string{"blocked-bot"}})
	req := httptest.NewRequest(http.MethodGet, "/ua", nil)
	req.Header.Set("User-Agent", "blocked-bot")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("ua-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"Not allowed"}` {
		t.Fatalf("body = %q", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestAllowlistMissFallsThrough(t *testing.T) {
	p := newTestPlugin(t, Config{AllowList: []string{"allowed-bot"}})
	req := httptest.NewRequest(http.MethodGet, "/ua", nil)
	req.Header.Set("User-Agent", "other-bot")

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
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestAllowlistWinsBeforeDenylist(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowList: []string{"same-bot"},
		DenyList:  []string{"same-bot"},
	})
	req := httptest.NewRequest(http.MethodGet, "/ua", nil)
	req.Header.Set("User-Agent", "same-bot")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestUserAgentIsTrimmedBeforeMatching(t *testing.T) {
	p := newTestPlugin(t, Config{DenyList: []string{"blocked-bot"}})
	req := httptest.NewRequest(http.MethodGet, "/ua", nil)
	req.Header.Set("User-Agent", "  blocked-bot  ")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("ua-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func newTestPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()

	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}
