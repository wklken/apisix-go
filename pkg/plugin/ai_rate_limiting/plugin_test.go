package ai_rate_limiting

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestPlugin(t *testing.T, cfg Config, now func() time.Time) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg, now: now}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerChargesTotalTokensAndRejectsNextRequest(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		Limit:         10,
		TimeWindow:    60,
		RejectedCode:  http.StatusTooManyRequests,
		RejectedMsg:   "token quota exceeded",
		LimitStrategy: "total_tokens",
	}, func() time.Time { return now })

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":4,"completion_tokens":8,"total_tokens":12}}`))
	})

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-RateLimit-Limit-global"); got != "10" {
		t.Fatalf("limit header = %q, want 10", got)
	}
	if got := first.Header().Get("X-AI-RateLimit-Remaining-global"); got != "0" {
		t.Fatalf("remaining header = %q, want 0 after charging response tokens", got)
	}

	second := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second response code = %d, want 429", second.Code)
	}
	if !strings.Contains(second.Body.String(), "token quota exceeded") {
		t.Fatalf("second response body = %q, want custom rejection message", second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want only first request to pass", calls)
	}
}

func TestHandlerUsesInstancePromptTokenQuota(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		LimitStrategy: "prompt_tokens",
		Instances: []InstanceLimit{
			{Name: "deepseek-main", Limit: 5, TimeWindow: 30},
		},
	}, func() time.Time { return now })

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":20,"total_tokens":23}}`))
	})

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		req := WithPickedAIInstanceName(
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			"deepseek-main",
		)
		p.Handler(upstream).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("response %d code = %d, want 200", i+1, rr.Code)
		}
	}

	rr := httptest.NewRecorder()
	req := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "deepseek-main")
	p.Handler(upstream).ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("third response code = %d, want default 503", rr.Code)
	}
	if got := rr.Header().Get("X-AI-RateLimit-Limit-deepseek-main"); got != "5" {
		t.Fatalf("instance limit header = %q, want 5", got)
	}
}

func TestHandlerSkipsUnconfiguredInstance(t *testing.T) {
	p := newTestPlugin(t, Config{
		LimitStrategy: "total_tokens",
		Instances: []InstanceLimit{
			{Name: "limited", Limit: 1, TimeWindow: 60},
		},
	}, time.Now)

	req := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "other")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":100}}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want pass-through for unconfigured instance", rr.Code)
	}
}

func TestHandlerResetsQuotaAfterWindow(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		Limit:      1,
		TimeWindow: 1,
	}, func() time.Time { return now })

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	p.Handler(upstream).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, httptest.NewRequest(http.MethodPost, "/", nil))
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}

	now = now.Add(2 * time.Second)
	allowed := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(allowed, httptest.NewRequest(http.MethodPost, "/", nil))
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed response code = %d, want 200 after reset", allowed.Code)
	}
}
