package ai_proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRunRequestPhasePublishesConsumeOnceProtocolOwner(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL},
	})
	r := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"ping"}]}`),
	)
	r.Header.Set("Content-Type", "application/json")
	r, _ = apisixctx.EnsureRequestLifecycle(r, timeNow())

	result := p.RunRequestPhase(httptest.NewRecorder(), r)
	if result.Decision != base.RequestContinue || result.Request == nil {
		t.Fatalf("RunRequestPhase() = %+v, want a continuing request", result)
	}
	if ai_runtime.FromRequest(result.Request) == nil {
		t.Fatal("RunRequestPhase() did not publish request-local AI execution")
	}

	called := 0
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called++ })
	first := httptest.NewRecorder()
	if _, _, _, err := p.RunExclusiveProtocol(first, result.Request, next); err != nil {
		t.Fatalf("first RunExclusiveProtocol() error = %v", err)
	}
	second := httptest.NewRecorder()
	if _, _, _, err := p.RunExclusiveProtocol(second, result.Request, next); err != nil {
		t.Fatalf("second RunExclusiveProtocol() error = %v", err)
	}
	if called != 0 || first.Code != http.StatusOK || second.Code != http.StatusOK ||
		first.Body.String() == "" || second.Body.String() != "" {
		t.Fatalf("calls=%d first=%d second=%d, want one consume-only protocol owner", called, first.Code, second.Code)
	}
}

func timeNow() time.Time { return time.Unix(0, 0) }
