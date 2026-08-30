package pluginintegration

import "time"

const differentialProxyBufferingSSEPolicy = "proxy-buffering-incremental-sse"

type differentialProxyBufferingStreamCase struct {
	Spec     DifferentialCase
	Contract differentialSSEStreamContract
}

func newDifferentialProxyBufferingStreamCase(spec DifferentialCase) differentialProxyBufferingStreamCase {
	return differentialProxyBufferingStreamCase{
		Spec: spec,
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
	}
}
