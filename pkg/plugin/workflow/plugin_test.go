package workflow

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
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

func TestParseOfficialReturnActionArray(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"case": []any{[]any{"uri", "==", "/anything/rejected"}},
				"actions": []any{
					[]any{"return", map[string]any{"code": 403}},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(cfg.Rules) != 1 || len(cfg.Rules[0].Actions) != 1 {
		t.Fatalf("rules = %#v, want one rule with one action", cfg.Rules)
	}
	action := cfg.Rules[0].Actions[0]
	if action.Name != "return" {
		t.Fatalf("action name = %q, want return", action.Name)
	}
	if action.Return.Code != http.StatusForbidden {
		t.Fatalf("return code = %d, want 403", action.Return.Code)
	}
}

func TestHandlerReturnsConfiguredStatusForMatchingCase(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"uri", "==", "/anything/rejected"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything/rejected", nil)
	rr := httptest.NewRecorder()
	nextCalled := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if nextCalled {
		t.Fatal("next handler was called, want workflow return to stop request")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "rejected by workflow") {
		t.Fatalf("body = %q, want workflow rejection message", rr.Body.String())
	}
}

func TestHandlerFallsThroughWhenNoCaseMatches(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"arg_name", "==", "blocked"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=allowed", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Called", "yes")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if rr.Header().Get("X-Next-Called") != "yes" {
		t.Fatal("next handler was not called")
	}
}

func TestHandlerUsesFirstMatchingRule(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"arg_name", "==", "blocked"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
			{
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusTeapot}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=blocked", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want first matching rule status 403", rr.Code)
	}
}
