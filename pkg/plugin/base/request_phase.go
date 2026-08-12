package base

import (
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type RequestDecision uint8

const (
	RequestContinue RequestDecision = iota
	RequestStop
)

type RequestPhaseResult struct {
	Request  *http.Request
	Decision RequestDecision
	Source   apisixctx.ResponseSource
}

func ContinueRequest(r *http.Request) RequestPhaseResult {
	return RequestPhaseResult{
		Request:  r,
		Decision: RequestContinue,
		Source:   apisixctx.ResponseSourceUnknown,
	}
}

func StopRequest(r *http.Request) RequestPhaseResult {
	return StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
}

func StopRequestWithSource(r *http.Request, source apisixctx.ResponseSource) RequestPhaseResult {
	return RequestPhaseResult{
		Request:  r,
		Decision: RequestStop,
		Source:   source,
	}
}

type RequestPhasePlugin interface {
	RunRequestPhase(http.ResponseWriter, *http.Request) RequestPhaseResult
}

func AdaptRequestPhase(plugin RequestPhasePlugin, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lifecycle := apisixctx.GetRequestLifecycle(r)
		result := plugin.RunRequestPhase(w, r)
		request := result.Request
		if request == nil {
			request = r
		}
		if lifecycle == nil {
			lifecycle = apisixctx.GetRequestLifecycle(request)
		}
		if lifecycle != nil {
			lifecycle.SetFinalRequest(request)
		}

		switch result.Decision {
		case RequestContinue:
			next.ServeHTTP(w, request)
		case RequestStop:
			apisixctx.SetRequestResponseSource(request, normalizeStopSource(result.Source))
		default:
			apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceEarlyStop)
		}
	})
}

func normalizeStopSource(source apisixctx.ResponseSource) apisixctx.ResponseSource {
	switch source {
	case apisixctx.ResponseSourceUpstream,
		apisixctx.ResponseSourceAPISIX,
		apisixctx.ResponseSourceEarlyStop,
		apisixctx.ResponseSourceCacheHit:
		return source
	default:
		return apisixctx.ResponseSourceEarlyStop
	}
}
