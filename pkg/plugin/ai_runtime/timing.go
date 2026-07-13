package ai_runtime

import (
	"net/http"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func StartLLMRequest(r *http.Request) time.Time {
	started := time.Now()
	if apisixctx.GetRequestVars(r) != nil {
		apisixctx.RegisterRequestVar(r, "$llm_request_start_time", unixSeconds(started))
		apisixctx.RegisterRequestVar(r, "$llm_request_done", false)
	}
	return started
}

func MarkFirstToken(r *http.Request, started time.Time) {
	if apisixctx.GetRequestVars(r) == nil || apisixctx.GetRequestVar(r, "$llm_time_to_first_token") != nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$llm_time_to_first_token", elapsedMilliseconds(started))
}

func MarkLLMRequestDone(r *http.Request, started time.Time) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$apisix_upstream_response_time", elapsedMilliseconds(started))
	apisixctx.RegisterRequestVar(r, "$llm_request_done", true)
}

func unixSeconds(value time.Time) float64 {
	return float64(value.UnixNano()) / float64(time.Second)
}

func elapsedMilliseconds(started time.Time) int64 {
	elapsed := time.Since(started).Milliseconds()
	if elapsed < 0 {
		return 0
	}
	return elapsed
}
