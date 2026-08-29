package pluginintegration

import (
	"strings"
	"testing"
)

func TestCompareDifferentialMQTTProxyCONNECTAcceptsExactProtocolExchange(t *testing.T) {
	spec := differentialMQTTProxyCases()[0].Spec
	candidate := exactDifferentialMQTTProxyObservation(spec, "127.0.0.1:36117")
	oracle := exactDifferentialMQTTProxyObservation(spec, "host.containers.internal:36117")
	passed, detail, err := compareDifferentialMQTTProxyCONNECT(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || detail != "" {
		t.Fatalf("comparison = passed %v, detail %q, err %v", passed, detail, err)
	}
}

func TestCompareDifferentialMQTTProxyCONNECTRejectsWeakenedSemantics(t *testing.T) {
	spec := differentialMQTTProxyCases()[0].Spec
	exact := exactDifferentialMQTTProxyObservation(spec, "127.0.0.1:36117")
	tests := []struct {
		name   string
		mutate func(*DifferentialObservation)
		want   string
	}{
		{
			name: "invalid header reached upstream",
			mutate: func(observation *DifferentialObservation) {
				observation.UpstreamCalls = []DifferentialUpstreamObservation{{
					Received: true, Fixture: spec.Fixture.Name, Method: "MQTT", Path: "INVALID", Body: "mmm",
				}, observation.Upstream}
			},
			want: "exactly one",
		},
		{
			name: "wrong CONNECT client id",
			mutate: func(observation *DifferentialObservation) {
				packet := []byte(observation.Upstream.Body)
				packet[len(packet)-1] = 'x'
				observation.Upstream.Body = string(packet)
				observation.UpstreamCalls[0] = observation.Upstream
			},
			want: "pinned CONNECT",
		},
		{
			name: "invalid packet was echoed",
			mutate: func(observation *DifferentialObservation) {
				observation.Steps[0].Body = "hello world"
			},
			want: "invalid-header",
		},
		{
			name: "valid packet response changed",
			mutate: func(observation *DifferentialObservation) {
				observation.Steps[1].Body = "wrong"
			},
			want: "forwarded CONNECT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := copyDifferentialObservation(exact)
			tt.mutate(&candidate)
			passed, _, err := compareDifferentialMQTTProxyCONNECT(
				spec, candidate, exact, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("comparison = passed %v, err %v, want error containing %q", passed, err, tt.want)
			}
		})
	}
}

func exactDifferentialMQTTProxyObservation(
	spec DifferentialCase,
	upstreamAddress string,
) DifferentialObservation {
	packet := spec.Steps[1].Request.Body
	upstream := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: "MQTT", Path: "CONNECT", Body: packet,
	}
	return DifferentialObservation{
		Steps: []DifferentialStepObservation{
			{Headers: map[string][]string{}, SecurityDecision: "not_applicable"},
			{Headers: map[string][]string{}, Body: "hello world", SecurityDecision: "not_applicable"},
		},
		Headers:         map[string][]string{},
		UpstreamFixture: spec.Fixture.Name,
		UpstreamAddress: upstreamAddress,
		Upstream:        upstream,
		UpstreamCalls:   []DifferentialUpstreamObservation{upstream},
	}
}
