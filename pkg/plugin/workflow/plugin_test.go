package workflow

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestHandlerRunsLimitCountAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-count",
						map[string]any{
							"count":         1,
							"time_window":   60,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

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

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
}

func TestHandlerRunsLimitReqAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-req",
						map[string]any{
							"rate":          1,
							"burst":         0,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
							"nodelay":       true,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.10:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.10:5678"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
}

func TestHandlerRunsLimitConnAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-conn",
						map[string]any{
							"conn":               1,
							"burst":              0,
							"default_conn_delay": 0.1,
							"key":                "remote_addr",
							"rejected_code":      http.StatusTooManyRequests,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		handler.ServeHTTP(firstRecorder, first)
	}()
	<-started

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:5678"
	secondRecorder := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		handler.ServeHTTP(secondRecorder, second)
	}()
	select {
	case <-secondDone:
	case <-time.After(200 * time.Millisecond):
		close(block)
		<-firstDone
		<-secondDone
		t.Fatal(
			"second request reached downstream, want workflow limit-conn action to reject while first request is active",
		)
	}
	if secondRecorder.Code != http.StatusTooManyRequests {
		close(block)
		<-firstDone
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}

	close(block)
	<-firstDone
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}
}
