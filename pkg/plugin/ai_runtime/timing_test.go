package ai_runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestLLMTimingLifecycle(t *testing.T) {
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/", nil))
	started := StartLLMRequest(req)
	time.Sleep(time.Millisecond)
	MarkFirstToken(req, started)
	first := apisixctx.GetRequestVar(req, "$llm_time_to_first_token")
	MarkFirstToken(req, started.Add(-time.Hour))
	MarkLLMRequestDone(req, started)

	if apisixctx.GetRequestVar(req, "$llm_request_start_time") == nil || first == nil ||
		apisixctx.GetRequestVar(req, "$llm_time_to_first_token") != first ||
		apisixctx.GetRequestVar(req, "$apisix_upstream_response_time") == nil ||
		apisixctx.GetRequestVar(req, "$llm_request_done") != true {
		t.Fatalf("timing vars = %#v", apisixctx.GetRequestVars(req))
	}
}
