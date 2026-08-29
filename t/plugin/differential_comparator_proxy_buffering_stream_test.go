package pluginintegration

import (
	"strings"
	"testing"
)

func TestCompareDifferentialProxyBufferingSSEAcceptsExactIncrementalStreams(t *testing.T) {
	streamCase := differentialProxyBufferingStreamingCases()[0]
	observation := exactDifferentialProxyBufferingSSEObservation(streamCase)
	passed, reason, err := compareDifferentialProxyBufferingSSE(streamCase, observation, observation)
	if err != nil || !passed || reason != "" {
		t.Fatalf("comparison = passed %v, reason %q, err %v", passed, reason, err)
	}
}

func TestCompareDifferentialProxyBufferingSSEEnvelopeUsesStreamingTranscript(t *testing.T) {
	streamCase := differentialProxyBufferingStreamingCases()[0]
	stream := exactDifferentialProxyBufferingSSEObservation(streamCase)
	candidate, err := differentialSSEObservationEnvelope(streamCase.Spec, stream)
	if err != nil {
		t.Fatalf("candidate envelope: %v", err)
	}
	oracle, err := differentialSSEObservationEnvelope(streamCase.Spec, stream)
	if err != nil {
		t.Fatalf("oracle envelope: %v", err)
	}
	passed, reason, err := compareDifferentialCaseObservations(
		streamCase.Spec, candidate, oracle, NormalizationPolicy{},
	)
	if err != nil || !passed || reason != "" {
		t.Fatalf("comparison = passed %v, reason %q, err %v", passed, reason, err)
	}

	candidate.Headers["X-Buffered"] = []string{"true"}
	if _, _, err := compareDifferentialCaseObservations(
		streamCase.Spec, candidate, oracle, NormalizationPolicy{},
	); err == nil {
		t.Fatal("stream comparator accepted an unmodeled envelope header")
	}
}

func TestCompareDifferentialProxyBufferingSSERejectsCompletedOrMutatedStreams(t *testing.T) {
	streamCase := differentialProxyBufferingStreamingCases()[0]
	exact := exactDifferentialProxyBufferingSSEObservation(streamCase)

	tests := []struct {
		name   string
		mutate func(*differentialSSEStreamObservation)
		want   string
	}{
		{
			name: "merged completed body",
			mutate: func(observation *differentialSSEStreamObservation) {
				observation.Frames = []string{strings.Join(streamCase.Contract.Frames, "")}
			},
			want: "frames",
		},
		{
			name: "reordered frames",
			mutate: func(observation *differentialSSEStreamObservation) {
				observation.Frames[0], observation.Frames[1] = observation.Frames[1], observation.Frames[0]
			},
			want: "frames",
		},
		{
			name: "EOF after third frame",
			mutate: func(observation *differentialSSEStreamObservation) {
				observation.ConnectionOpenAfterRequiredFrames = false
			},
			want: "EOF",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := exact
			candidate.Frames = append([]string(nil), exact.Frames...)
			tt.mutate(&candidate)
			passed, _, err := compareDifferentialProxyBufferingSSE(streamCase, candidate, exact)
			if err == nil || passed || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("comparison = passed %v, err %v, want error containing %q", passed, err, tt.want)
			}
		})
	}
}

func exactDifferentialProxyBufferingSSEObservation(
	streamCase differentialProxyBufferingStreamCase,
) differentialSSEStreamObservation {
	return differentialSSEStreamObservation{
		Status:                            streamCase.Spec.Fixture.Response.Status,
		ContentType:                       streamCase.Spec.Fixture.Response.Headers["Content-Type"],
		Frames:                            append([]string(nil), streamCase.Contract.Frames...),
		ConnectionOpenAfterRequiredFrames: true,
	}
}
