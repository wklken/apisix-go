package traffic_label

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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

func TestHandlerSetsHeadersForFirstMatchingRule(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []any{[]any{"arg_version", "==", "v1"}},
				Actions: []Action{
					{SetHeaders: map[string]any{"X-Server-Id": "100", "X-Version": "$arg_version"}},
				},
			},
			{
				Actions: []Action{
					{SetHeaders: map[string]any{"X-Server-Id": "fallback"}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?version=v1", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Server-Id"); got != "100" {
			t.Fatalf("X-Server-Id = %q, want 100", got)
		}
		if got := r.Header.Get("X-Version"); got != "v1" {
			t.Fatalf("X-Version = %q, want v1", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerSkipsWhenNoRuleMatches(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []any{[]any{"arg_version", "==", "v1"}},
				Actions: []Action{
					{SetHeaders: map[string]any{"X-Server-Id": "100"}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?version=v2", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Server-Id"); got != "" {
			t.Fatalf("X-Server-Id = %q, want empty", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerUsesWeightedActions(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []any{[]any{"uri", "==", "/anything"}},
				Actions: []Action{
					{SetHeaders: map[string]any{"X-Bucket": "blue"}, Weight: 2},
					{SetHeaders: map[string]any{"X-Bucket": "green"}, Weight: 1},
					{Weight: 1},
				},
			},
		},
	})

	seen := map[string]int{}
	for range 4 {
		req := httptest.NewRequest(http.MethodGet, "/anything", nil)
		rr := httptest.NewRecorder()

		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen[r.Header.Get("X-Bucket")]++
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rr.Code)
		}
	}

	if seen["blue"] != 2 {
		t.Fatalf("blue selections = %d, want 2; all = %v", seen["blue"], seen)
	}
	if seen["green"] != 1 {
		t.Fatalf("green selections = %d, want 1; all = %v", seen["green"], seen)
	}
	if seen[""] != 1 {
		t.Fatalf("pass-through selections = %d, want 1; all = %v", seen[""], seen)
	}
}

func TestMatchSupportsRequestHeaderAndNotEquals(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []any{
					[]any{"http_x_region", "==", "west"},
					[]any{"arg_skip", "!=", "true"},
				},
				Actions: []Action{
					{SetHeaders: map[string]any{"X-Traffic-Label": "$http_x_region"}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?skip=false", nil)
	req.Header.Set("X-Region", "west")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Traffic-Label"); got != "west" {
			t.Fatalf("X-Traffic-Label = %q, want west", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestMatchSupportsPrefixedVarsNumericAndRegexOperators(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []any{
					[]any{"$request_method", "==", http.MethodGet},
					[]any{"arg_score", ">=", "10"},
					[]any{"http_x_region", "~", "^west-[0-9]+$"},
					[]any{"uri", "!~", "/internal"},
				},
				Actions: []Action{
					{SetHeaders: map[string]any{"X-Traffic-Label": "$arg_score-$http_x_region"}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?score=12", nil)
	req.Header.Set("X-Region", "west-1")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Traffic-Label"); got != "12-west-1" {
			t.Fatalf("X-Traffic-Label = %q, want 12-west-1", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerSupportsNestedRestyExpressionAndApisixVariables(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []any{
					"AND",
					[]any{"request_method", "in", []any{"GET", "HEAD"}},
					[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
					[]any{"http_x_env", "~*", "^prod$"},
					[]any{"graphql_root_fields", "has", "owner"},
					[]any{"arg_skip", "!", "==", "yes"},
				},
				Actions: []Action{{SetHeaders: map[string]any{"X-Traffic-Label": "$graphql_root_fields"}}},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?skip=no", nil)
	req.RemoteAddr = "192.0.2.40:12345"
	req.Header.Set("X-Env", "PrOd")
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$graphql_root_fields", []string{"viewer", "owner"})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Traffic-Label"); got != "viewer,owner" {
			t.Fatalf("X-Traffic-Label = %q, want viewer,owner", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
}

func TestPostInitRejectsInvalidMatchExpression(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{{
		Match:   []any{[]any{"uri", "bogus", "/anything"}},
		Actions: []Action{{SetHeaders: map[string]any{"X-Traffic-Label": "bad"}}},
	}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid match rejected")
	}
}

func TestConfigAcceptsNumericSetHeaderValues(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"rules": []any{map[string]any{
			"actions": []any{map[string]any{
				"set_headers": map[string]any{"X-Server-Id": 100},
			}},
		}},
	}, p.Config()); err != nil {
		t.Fatalf("Parse() numeric set_headers error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Server-Id"); got != "100" {
			t.Fatalf("X-Server-Id = %q, want 100", got)
		}
	})).ServeHTTP(httptest.NewRecorder(), req)
}

func TestSchemaValidatesOfficialTrafficLabelShape(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	valid := map[string]any{
		"rules": []any{map[string]any{
			"match": []any{[]any{"uri", "==", "/anything"}},
			"actions": []any{map[string]any{
				"set_headers": map[string]any{"X-Server-Id": 100},
			}},
		}},
	}
	if err := util.Validate(valid, p.GetSchema()); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}

	invalid := []map[string]any{
		{"rules": []any{map[string]any{"match": []any{}, "actions": []any{map[string]any{"weight": 1}}}}},
		{"rules": []any{map[string]any{"actions": []any{map[string]any{"add_headers": map[string]any{"X": "y"}}}}}},
		{"rules": []any{map[string]any{"actions": []any{map[string]any{"set_headers": map[string]any{"X": true}}}}}},
	}
	for _, config := range invalid {
		if err := util.Validate(config, p.GetSchema()); err == nil {
			t.Fatalf("Validate(%v) error = nil, want invalid config rejected", config)
		}
	}
}
