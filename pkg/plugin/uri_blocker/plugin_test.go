package uri_blocker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestBlockedURIDefaultResponseHasNoBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BlockRules: []string{`^/blocked`},
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/blocked?a=1", nil))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if got := rr.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty", got)
	}
}

func TestBlockedURICustomMessageUsesErrorMessageJSON(t *testing.T) {
	p := newTestPlugin(t, Config{
		BlockRules:   []string{`^/blocked`},
		RejectedMsg:  "blocked by uri",
		RejectedCode: http.StatusTeapot,
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/blocked", nil))

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTeapot)
	}
	if got := rr.Body.String(); got != "{\"error_msg\":\"blocked by uri\"}\n" {
		t.Fatalf("body = %q, want APISIX 3.17 JSON response with trailing newline", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX 3.17 response type", got)
	}
}

func TestCaseInsensitiveMatch(t *testing.T) {
	caseInsensitive := true
	p := newTestPlugin(t, Config{
		BlockRules:      []string{`^/blocked`},
		CaseInsensitive: &caseInsensitive,
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/BLOCKED", nil))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestPostInitRejectsInvalidRegularExpression(t *testing.T) {
	p := &Plugin{config: Config{BlockRules: []string{`.+(`}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid regular expression rejected")
	}
}

func TestSchemaRejectsEmptyBlockRules(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{"block_rules": []any{}}, p.GetSchema()); err == nil {
		t.Fatal("empty block_rules should fail schema validation")
	}
}

func TestPostInitRejectsEmptyBlockRules(t *testing.T) {
	p := &Plugin{config: Config{BlockRules: []string{}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want empty block_rules rejected")
	}
}

func TestSchemaRejectsRejectedCodeAboveHTTPMaximum(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"block_rules":   []any{"^/blocked"},
		"rejected_code": 1000,
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("rejected_code=1000 should fail schema validation")
	}
}

func TestSchemaAcceptsAPISIX317ThreeDigitNonstandardRejectedCodes(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, rejectedCode := range []int{600, 999} {
		config := map[string]any{
			"block_rules":   []any{"^/blocked"},
			"rejected_code": rejectedCode,
		}
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("rejected_code=%d failed schema validation: %v", rejectedCode, err)
		}
	}
}

func TestHandlerMatchesOriginalRawRequestURI(t *testing.T) {
	p := newTestPlugin(t, Config{BlockRules: []string{`%2Fprivate\?token=%2f`}})
	req := httptest.NewRequest(http.MethodGet, "/decoded/private?token=/", nil)
	req.RequestURI = "/encoded%2Fprivate?token=%2f"
	req.URL.Path = "/decoded/private"
	req.URL.RawPath = ""
	req.URL.RawQuery = "token=/"

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should match the original raw request URI")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestNormalizedPathCannotBypassAnchoredRule(t *testing.T) {
	p := newTestPlugin(t, Config{BlockRules: []string{`^/internal/`}})
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{Config: config.Config{
		Apisix: config.Apisix{NormalizeURILikeServlet: true},
	}}})
	req := httptest.NewRequest(http.MethodGet, "/./internal/x?aa=1", nil)
	req.URL.Path = "/internal/x"

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestAllowedURIFallsThrough(t *testing.T) {
	p := newTestPlugin(t, Config{
		BlockRules: []string{`^/blocked`},
	})

	called := false
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/allowed", nil))

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
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
