package redirect

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestRunRequestPhaseStopsWithEarlyStopSource(t *testing.T) {
	p := newTestPlugin(t, Config{Uri: "/target"})
	request := httptest.NewRequest(http.MethodGet, "/source", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	response := httptest.NewRecorder()

	result := p.RunRequestPhase(response, request)
	if result.Decision != 1 {
		t.Fatalf("decision = %v, want stop", result.Decision)
	}
	if result.Source != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("source = %q, want early_stop", result.Source)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("lifecycle source = %q, want early_stop", lifecycle.ResponseSource())
	}
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/target" {
		t.Fatalf("response = %d/%q, want 302/target", response.Code, response.Header().Get("Location"))
	}
}

func TestRunRequestPhaseContinuesWhenRegexDoesNotMatch(t *testing.T) {
	p := newTestPlugin(t, Config{RegexUri: []string{`^/users/(\\d+)$`, `/profiles/$1`}})
	result := p.RunRequestPhase(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders/1", nil))
	if result.Decision != 0 {
		t.Fatalf("decision = %v, want continue", result.Decision)
	}
}
