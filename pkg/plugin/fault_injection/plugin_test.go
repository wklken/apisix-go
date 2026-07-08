package fault_injection

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestAbortVarsMustMatch(t *testing.T) {
	body := "aborted"
	p := newTestPlugin(t, Config{
		Abort: &Abort{
			HTTPStatus: http.StatusServiceUnavailable,
			Body:       &body,
			Vars: [][]interface{}{
				{"arg_stage", "==", "beta"},
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
			Vars: [][]interface{}{
				{"arg_stage", "==", "beta"},
				{"http_x_fault", "==", "on"},
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
			Vars: [][]interface{}{
				{"arg_stage", "==", "beta"},
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
