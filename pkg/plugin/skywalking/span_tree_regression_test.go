package skywalking

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// Entry (spanId 0) plus Exit (spanId 1). Injected sw8 parent span id is 1
// and must exist in the reported segment.
func TestSkyWalkingExitSpan(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var segments []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&segments); err != nil {
			t.Fatalf("decode segments: %v", err)
		}
		reported <- segments
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{EndpointAddr: server.URL, SampleRatio: 1})
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/opentracing", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("decision = %d, want continue", result.Decision)
	}
	injected, ok := parseSW8(result.Request.Header.Get("sw8"))
	if !ok {
		t.Fatalf("injected sw8 = %q", result.Request.Header.Get("sw8"))
	}
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("finalize failures = %#v", failures)
	}
	p.Flush()
	select {
	case segments := <-reported:
		if len(segments) != 1 {
			t.Fatalf("segments = %d, want 1", len(segments))
		}
		spans, _ := segments[0]["spans"].([]any)
		ids := map[int]bool{}
		for _, raw := range spans {
			span, _ := raw.(map[string]any)
			id, _ := span["spanId"].(float64)
			ids[int(id)] = true
		}
		if !ids[injected.ParentSpanID] {
			t.Fatalf(
				"reported span ids = %v, injected sw8 parent span id = %d is missing (APISIX 3.17 Exit span)",
				ids,
				injected.ParentSpanID,
			)
		}
		if len(spans) < 2 {
			t.Fatalf("span count = %d, want Entry+Exit (>=2)", len(spans))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for skywalking report")
	}
}
