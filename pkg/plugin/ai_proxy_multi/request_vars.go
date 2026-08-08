package ai_proxy_multi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func registerUpstreamTargetVars(r *http.Request, upstream *http.Request) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$upstream_addr", upstream.URL.Host)
	apisixctx.RegisterRequestVar(r, "$upstream_uri", upstream.URL.RequestURI())
	apisixctx.RegisterRequestVar(r, "$upstream_host", upstream.URL.Hostname())
}

func registerUpstreamResponseVars(r *http.Request, status int, elapsed time.Duration, responseLength int64) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$upstream_status", strconv.Itoa(status))
	registerUpstreamResponseTime(r, elapsed)
	apisixctx.RegisterRequestVar(r, "$upstream_response_length", responseLength)
}

func registerUpstreamResponseTime(r *http.Request, elapsed time.Duration) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$upstream_response_time", fmt.Sprintf("%.3f", elapsed.Seconds()))
}
