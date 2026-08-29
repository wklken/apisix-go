package pluginintegration

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCompareDifferentialMCPBridgeSSESessionNormalizesOnlyTransportAndSessionIdentity(t *testing.T) {
	spec := differentialMCPBridgeCases()[0]
	candidate := differentialMCPBridgeObservationForTest(t, "11111111-2222-4333-8444-555555555555")
	oracle := differentialMCPBridgeObservationForTest(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	candidate.Headers["Date"] = []string{"candidate-date"}
	oracle.Headers["Server"] = []string{"APISIX"}
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialMCPBridgeSSESession(
		spec, candidate, oracle, mcpBridgeNormalizationPolicyForTest(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare = passed %t, diff %q, err %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("comparator mutated its input observations")
	}
}

func TestCompareDifferentialMCPBridgeSSESessionRejectsSemanticMutations(t *testing.T) {
	spec := differentialMCPBridgeCases()[0]
	tests := []struct {
		name   string
		mutate func(*DifferentialObservation)
	}{
		{name: "post status", mutate: func(observation *DifferentialObservation) {
			observation.Steps[0].Status = 200
		}},
		{name: "upstream call", mutate: func(observation *DifferentialObservation) {
			observation.Upstream.Received = true
		}},
		{name: "wrong endpoint", mutate: func(observation *DifferentialObservation) {
			mutateMCPBridgeTranscript(t, observation, func(transcript *differentialMCPBridgeTranscript) {
				transcript.Endpoint.Data = "/wrong/message?sessionId=11111111-2222-4333-8444-555555555555"
			})
		}},
		{name: "wrong ping", mutate: func(observation *DifferentialObservation) {
			mutateMCPBridgeTranscript(t, observation, func(transcript *differentialMCPBridgeTranscript) {
				transcript.Ping.Data = `{"jsonrpc":"2.0","method":"other","id":"ping:1"}`
			})
		}},
		{name: "wrong message", mutate: func(observation *DifferentialObservation) {
			mutateMCPBridgeTranscript(t, observation, func(transcript *differentialMCPBridgeTranscript) {
				transcript.Message.Data = `{"jsonrpc":"2.0","id":8,"result":{}}`
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := differentialMCPBridgeObservationForTest(t, "11111111-2222-4333-8444-555555555555")
			test.mutate(&candidate)
			_, _, err := compareDifferentialMCPBridgeSSESession(
				spec,
				candidate,
				differentialMCPBridgeObservationForTest(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"),
				mcpBridgeNormalizationPolicyForTest(),
			)
			if err == nil {
				t.Fatal("compare error = nil, want semantic mutation rejection")
			}
		})
	}
}

func mcpBridgeNormalizationPolicyForTest() NormalizationPolicy {
	return NormalizationPolicy{
		SchemaVersion: 1,
		Headers: HeaderNormalizationPolicy{
			CanonicalizeNames:        true,
			Ignore:                   []string{"Date", "Server"},
			StripHopByHop:            true,
			ContentLengthIfBodyEqual: true,
		},
		FixtureEndpointMapping: true,
	}
}

func differentialMCPBridgeObservationForTest(t *testing.T, sessionID string) DifferentialObservation {
	t.Helper()
	encoded, err := json.Marshal(differentialMCPBridgeTranscript{
		Endpoint: differentialMCPBridgeSSEEvent{
			Event: "endpoint", Data: "/mcp/message?sessionId=" + sessionID,
		},
		Ping: differentialMCPBridgeSSEEvent{
			Event: "message", Data: `{"jsonrpc":"2.0","method":"ping","id":"ping:1"}`,
		},
		Message: differentialMCPBridgeSSEEvent{
			Event: "message", Data: differentialMCPBridgePostedPayload,
		},
	})
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	return DifferentialObservation{
		Status: 200,
		Headers: map[string][]string{
			"Content-Type":  {"text/event-stream"},
			"Cache-Control": {"no-cache"},
		},
		Body: string(encoded), Host: "gateway.example.test",
		SecurityDecision: "not_applicable",
		Steps: []DifferentialStepObservation{{
			Status: 202, Headers: map[string][]string{}, Body: "",
			Host: "gateway.example.test", SecurityDecision: "not_applicable",
		}},
	}
}

func mutateMCPBridgeTranscript(
	t *testing.T,
	observation *DifferentialObservation,
	mutate func(*differentialMCPBridgeTranscript),
) {
	t.Helper()
	transcript, err := decodeDifferentialMCPBridgeTranscript(observation.Body)
	if err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	mutate(&transcript)
	encoded, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	observation.Body = string(encoded)
}
