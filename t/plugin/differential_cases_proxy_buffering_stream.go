package pluginintegration

import (
	"net/http"
	"time"
)

const differentialProxyBufferingSSEPolicy = "proxy-buffering-incremental-sse"

type differentialProxyBufferingStreamCase struct {
	Spec        DifferentialCase
	Contract    differentialSSEStreamContract
	SourceFiles []string
}

// differentialProxyBufferingStreamingCases is intentionally separate from
// differentialCases until the shared runner can drive the candidate locally
// and the oracle from inside its network namespace. The old static-body case
// remains a transit-only check and is not streaming qualification evidence.
func differentialProxyBufferingStreamingCases() []differentialProxyBufferingStreamCase {
	const routeID = "differential-proxy-buffering-sse"
	return []differentialProxyBufferingStreamCase{{
		Spec: DifferentialCase{
			Name:             "proxy-buffering-disabled-incremental-sse",
			Plugin:           "proxy-buffering",
			RouteID:          routeID,
			ComparisonPolicy: differentialProxyBufferingSSEPolicy,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/events",
					"plugins": map[string]any{
						"proxy-buffering": map[string]any{
							"disable_proxy_buffering": true,
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/events",
				Host:   "gateway.example.test",
				Headers: map[string]string{
					"Accept": "text/event-stream",
				},
			},
			Fixture: DifferentialFixture{
				Name:            "primary",
				WireProtocol:    differentialFixtureWireSSEHTTP,
				ExpectedCalls:   0,
				SemanticHeaders: []string{"Accept"},
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Headers: map[string]string{
						"Content-Type": "text/event-stream",
					},
				},
			},
			SecurityDecision: "not_applicable",
		},
		Contract: differentialSSEStreamContract{
			Frames: []string{
				"data: event-1\n\n",
				"data: event-2\n\n",
				"data: event-3\n\n",
			},
			RequiredFrames:  3,
			InterFrameDelay: 25 * time.Millisecond,
			OpenProbeWindow: 50 * time.Millisecond,
		},
		SourceFiles: []string{
			"t/cli/test_proxy_buffering.sh",
			"t/cli/test_sse.py",
		},
	}}
}

func differentialProxyBufferingStreamingCaseSpecs() []DifferentialCase {
	streamCases := differentialProxyBufferingStreamingCases()
	cases := make([]DifferentialCase, len(streamCases))
	for index := range streamCases {
		cases[index] = streamCases[index].Spec
	}
	return cases
}
