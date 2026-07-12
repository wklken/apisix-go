package fault_injection

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestAbortPercentageZeroFallsThrough(t *testing.T) {
	percentage := 0
	body := "should not be used"
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
			Body:       &body,
			Percentage: &percentage,
		},
	})

	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/fault", nil))

	if !called {
		t.Fatal("next handler was not called")
	}
}

func TestAbortWithoutPercentageAlwaysAborts(t *testing.T) {
	body := "aborted"
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
			Body:       &body,
		},
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fault-injection should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/fault", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if got := rr.Body.String(); got != "aborted" {
		t.Fatalf("body = %q, want aborted", got)
	}
}

func TestAbortWithoutBodyDoesNotPanic(t *testing.T) {
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
		},
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fault-injection should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/fault", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if got := rr.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty body", got)
	}
}

func TestAbortSerializesNumericHeaderValues(t *testing.T) {
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
			Headers: map[string]any{
				"X-Retry-After": 3,
			},
		},
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fault-injection should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/fault", nil))

	if got := rr.Header().Get("X-Retry-After"); got != "3" {
		t.Fatalf("X-Retry-After = %q, want 3", got)
	}
}

func TestAbortVarsMustMatch(t *testing.T) {
	body := "aborted"
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
			Body:       &body,
			Vars: [][]any{
				{[]any{"arg_stage", "==", "beta"}},
			},
		},
	})

	called := false
	req := httptest.NewRequest(http.MethodGet, "/fault?stage=stable", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called for non-matching vars")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestAbortVarsUseAnyMatchingExpression(t *testing.T) {
	body := "aborted"
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
			Body:       &body,
			Vars: [][]any{
				{[]any{"arg_stage", "==", "beta"}},
				{[]any{"http_x_fault", "==", "on"}},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/fault?stage=stable", nil)
	req.Header.Set("X-Fault", "on")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fault-injection should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if got := rr.Body.String(); got != "aborted" {
		t.Fatalf("body = %q, want aborted", got)
	}
}

func TestAbortSupportsBoundedVarsAndVariableRendering(t *testing.T) {
	body := "blocked $arg_score $http_x_region"
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
			Body:       &body,
			Headers: map[string]any{
				"X-Fault": "$request_method-$arg_score",
			},
			Vars: [][]any{{
				[]any{"$request_method", "==", http.MethodGet},
				[]any{"arg_score", ">=", "10"},
				[]any{"http_x_region", "~", "^west-[0-9]+$"},
				[]any{"uri", "!~", "/internal"},
			}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/fault?score=12", nil)
	req.Header.Set("X-Region", "west-1")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fault-injection should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if got := rr.Header().Get("X-Fault"); got != "GET-12" {
		t.Fatalf("X-Fault = %q, want GET-12", got)
	}
	if got := rr.Body.String(); got != "blocked 12 west-1" {
		t.Fatalf("body = %q, want rendered body", got)
	}
}

func TestAbortVarsSupportBoundedOperators(t *testing.T) {
	tests := []struct {
		name string
		expr []any
	}{
		{name: "prefixed var", expr: []any{"$request_method", "==", http.MethodGet}},
		{name: "greater equal", expr: []any{"arg_score", ">=", "10"}},
		{name: "less than", expr: []any{"arg_score", "<", "20"}},
		{name: "regex", expr: []any{"http_x_region", "~", "^west-[0-9]+$"}},
		{name: "negative regex", expr: []any{"uri", "!~", "/internal"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Abort: &Abort{
					HTTPStatus: http.StatusServiceUnavailable,
					Vars:       [][]any{{tt.expr}},
				},
			})
			req := httptest.NewRequest(http.MethodGet, "/fault?score=12", nil)
			req.Header.Set("X-Region", "west-1")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("fault-injection should not call the next handler")
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d for %v", rr.Code, http.StatusServiceUnavailable, tt.expr)
			}
		})
	}
}

func TestAbortVarsSupportNestedRestyExpressionsAndApisixVariables(t *testing.T) {
	body := "aborted"
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
			Body:       &body,
			Vars: [][]any{
				{
					"AND",
					[]any{"request_method", "in", []any{"GET", "HEAD"}},
					[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
					[]any{"http_x_env", "~*", "^prod$"},
					[]any{"graphql_root_fields", "has", "owner"},
					[]any{"arg_skip", "!", "==", "yes"},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/fault?skip=no", nil)
	req.RemoteAddr = "192.0.2.40:12345"
	req.Header.Set("X-Env", "PrOd")
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$graphql_root_fields", []string{"viewer", "owner"})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fault-injection should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestPostInitRejectsInvalidVarsExpressions(t *testing.T) {
	tests := []Config{
		{Abort: &Abort{HTTPStatus: http.StatusServiceUnavailable, Vars: [][]any{{"status", "==", 200}}}},
		{Abort: &Abort{HTTPStatus: http.StatusServiceUnavailable, Vars: [][]any{{
			[]any{"status", "bogus", 200},
		}}}},
		{Delay: &Delay{Duration: 0.1, Vars: [][]any{{"AND", []any{"method", "==", "GET"}}}}},
	}
	for _, config := range tests {
		p := &Plugin{config: config}
		if err := p.Init(); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		if err := p.PostInit(); err == nil {
			t.Fatalf("PostInit(%+v) error = nil, want invalid vars rejected", config)
		}
	}
}

func TestDelayVarsMustMatch(t *testing.T) {
	oldSleep := sleep
	var sleeps []time.Duration
	sleep = func(d time.Duration) {
		sleeps = append(sleeps, d)
	}
	t.Cleanup(func() {
		sleep = oldSleep
	})

	p := newTestPlugin(t, Config{
		Delay: &Delay{
			Duration: 0.01,
			Vars: [][]any{
				{[]any{"arg_stage", "==", "beta"}},
			},
		},
	})

	stableReq := httptest.NewRequest(http.MethodGet, "/fault?stage=stable", nil)
	performDelayRequest(p, stableReq)
	if len(sleeps) != 0 {
		t.Fatalf("non-matching vars caused sleeps %#v, want none", sleeps)
	}

	betaReq := httptest.NewRequest(http.MethodGet, "/fault?stage=beta", nil)
	performDelayRequest(p, betaReq)

	if len(sleeps) != 1 {
		t.Fatalf("sleeps = %#v, want one delay", sleeps)
	}
	if sleeps[0] != 10*time.Millisecond {
		t.Fatalf("delay = %s, want 10ms fractional duration", sleeps[0])
	}
}

func performDelayRequest(p *Plugin, req *http.Request) {
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), req)
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
