package traffic_label

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

func TestHandlerSetsHeadersForFirstMatchingRule(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []any{[]any{"arg_version", "==", "v1"}},
				Actions: []Action{
					{SetHeaders: map[string]string{"X-Server-Id": "100", "X-Version": "$arg_version"}},
				},
			},
			{
				Actions: []Action{
					{SetHeaders: map[string]string{"X-Server-Id": "fallback"}},
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
					{SetHeaders: map[string]string{"X-Server-Id": "100"}},
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
					{SetHeaders: map[string]string{"X-Bucket": "blue"}, Weight: 2},
					{SetHeaders: map[string]string{"X-Bucket": "green"}, Weight: 1},
					{Weight: 1},
				},
			},
		},
	})

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
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
					{SetHeaders: map[string]string{"X-Traffic-Label": "$http_x_region"}},
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
					{SetHeaders: map[string]string{"X-Traffic-Label": "$arg_score-$http_x_region"}},
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
