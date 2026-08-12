package ai_rate_limiting

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestStreamingWrapperFinalizationChargesExactlyOnce(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{Limit: 2, TimeWindow: 60}, func() time.Time { return now })
	r := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/", nil))
	r = apisixctx.WithRequestLifecycle(r, apisixctx.NewRequestLifecycle(now))
	result := p.RunRequestPhase(httptest.NewRecorder(), r)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	apisixctx.RegisterRequestVar(result.Request, "$ai_token_usage", map[string]any{"total_tokens": int64(3)})
	wrapped, err := p.WrapStreamingResponse(httptest.NewRecorder(), result.Request)
	if err != nil {
		t.Fatalf("WrapStreamingResponse() error = %v", err)
	}
	if _, err := wrapped.Write([]byte("data: done\n\n")); err != nil {
		t.Fatalf("stream write error = %v", err)
	}
	finalizer, ok := wrapped.(base.StreamingResponseFinalizer)
	if !ok {
		t.Fatalf("wrapped writer %T does not expose StreamingResponseFinalizer", wrapped)
	}
	if err := finalizer.FinishStreamingResponse(nil); err != nil {
		t.Fatalf("first finalization error = %v", err)
	}
	if err := finalizer.FinishStreamingResponse(nil); err != nil {
		t.Fatalf("second finalization error = %v", err)
	}

	second := httptest.NewRecorder()
	result = p.RunRequestPhase(second, r)
	if result.Decision != base.RequestStop || second.Code != p.config.RejectedCode {
		t.Fatalf("second request decision=%v status=%d, want rejected after one charge", result.Decision, second.Code)
	}
}
