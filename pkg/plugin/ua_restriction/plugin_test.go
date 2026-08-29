package ua_restriction

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestUARestrictionPostInitRejectsInvalidRegex(t *testing.T) {
	for _, test := range []struct {
		name    string
		config  Config
		field   string
		pattern string
	}{
		{
			name: "allowlist",
			config: Config{
				AllowList: []string{"valid", "[invalid"},
			},
			field: "allowlist", pattern: "[invalid",
		},
		{
			name: "denylist",
			config: Config{
				DenyList: []string{"valid", "(?invalid"},
			},
			field: "denylist", pattern: "(?invalid",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &Plugin{config: test.config}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			err := p.PostInit()
			if err == nil {
				t.Fatal("PostInit() error = nil, want invalid regex rejection")
			}
			for _, want := range []string{test.field + "[1]", test.pattern} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("PostInit() error = %q, want %q", err, want)
				}
			}
		})
	}
}

func TestUARestrictionValidRegexListCompilesAllPatterns(t *testing.T) {
	p := newTestPlugin(t, Config{AllowList: []string{`^allowed-`, `-bot$`}})
	for _, userAgent := range []string{"allowed-client", "crawler-bot"} {
		req := httptest.NewRequest(http.MethodGet, "/ua", nil)
		req.Header.Set("User-Agent", userAgent)
		response := httptest.NewRecorder()

		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, req)

		if response.Code != http.StatusNoContent {
			t.Fatalf("User-Agent %q status = %d, want 204", userAgent, response.Code)
		}
	}
}

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
	if got := rr.Body.String(); got != "{\"message\":\"Not allowed\"}\n" {
		t.Fatalf("body = %q, want APISIX 3.17 JSON response with trailing newline", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX 3.17 response type", got)
	}
}

func TestAllowlistMissIsRejected(t *testing.T) {
	p := newTestPlugin(t, Config{AllowList: []string{"allowed-bot"}})
	req := httptest.NewRequest(http.MethodGet, "/ua", nil)
	req.Header.Set("User-Agent", "other-bot")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("ua-restriction should not call the next handler for an allowlist miss")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"Not allowed"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestUserAgentHeaderValuesAreMatchedIndividually(t *testing.T) {
	allowlist := Config{AllowList: []string{`^allowed-bot$`}}
	denylist := Config{DenyList: []string{`^blocked-bot$`}}

	for _, test := range []struct {
		name       string
		config     Config
		userAgents []string
		wantStatus int
	}{
		{name: "allowlist single match", config: allowlist, userAgents: []string{"allowed-bot"}, wantStatus: http.StatusNoContent},
		{name: "allowlist single miss", config: allowlist, userAgents: []string{"other-bot"}, wantStatus: http.StatusForbidden},
		{name: "allowlist match first", config: allowlist, userAgents: []string{"allowed-bot", "other-bot"}, wantStatus: http.StatusNoContent},
		{name: "allowlist match second", config: allowlist, userAgents: []string{"other-bot", "allowed-bot"}, wantStatus: http.StatusNoContent},
		{name: "allowlist empty then match", config: allowlist, userAgents: []string{"", "allowed-bot"}, wantStatus: http.StatusNoContent},
		{name: "allowlist only empty", config: allowlist, userAgents: []string{""}, wantStatus: http.StatusForbidden},
		{name: "denylist single match", config: denylist, userAgents: []string{"blocked-bot"}, wantStatus: http.StatusForbidden},
		{name: "denylist single miss", config: denylist, userAgents: []string{"other-bot"}, wantStatus: http.StatusNoContent},
		{name: "denylist match first", config: denylist, userAgents: []string{"blocked-bot", "other-bot"}, wantStatus: http.StatusForbidden},
		{name: "denylist match second", config: denylist, userAgents: []string{"other-bot", "blocked-bot"}, wantStatus: http.StatusForbidden},
		{name: "denylist empty then match", config: denylist, userAgents: []string{"", "blocked-bot"}, wantStatus: http.StatusForbidden},
		{name: "denylist only empty", config: denylist, userAgents: []string{""}, wantStatus: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, test.config)
			req := httptest.NewRequest(http.MethodGet, "/ua", nil)
			for _, userAgent := range test.userAgents {
				req.Header.Add("User-Agent", userAgent)
			}

			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, test.wantStatus)
			}
		})
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
