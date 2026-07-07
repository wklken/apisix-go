package fault_injection

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
