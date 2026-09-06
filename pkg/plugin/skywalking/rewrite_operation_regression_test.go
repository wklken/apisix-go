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

func TestExitSpanKeepsRewritePhaseURI(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var segments []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&segments); err != nil {
			t.Error(err)
			return
		}
		reported <- segments
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()
	p := newTestPlugin(t, Config{EndpointAddr: collector.URL, SampleRatio: 1})
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/original", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("decision = %d", result.Decision)
	}
	rewritten := result.Request.Clone(result.Request.Context())
	rewritten.URL.Path = "/rewritten"
	lifecycle.SetFinalRequest(rewritten)
	lifecycle.Complete(apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: 200}, time.Now())
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatal(failures)
	}
	p.Flush()
	select {
	case segments := <-reported:
		spans, _ := segments[0]["spans"].([]any)
		exit, _ := spans[1].(map[string]any)
		if got := exit["operationName"]; got != "/original" {
			t.Fatalf(
				"Exit operationName = %v, want rewrite-phase URI /original (APISIX 3.17 tracer captures ngx.var.uri in rewrite)",
				got,
			)
		}
	case <-time.After(time.Second):
		t.Fatal("no segment")
	}
}
