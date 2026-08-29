package pluginintegration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
)

func TestCompareDifferentialKafkaProxyPubSubRecordAcceptsExactTranscript(t *testing.T) {
	spec := differentialKafkaProxyCases()[0]
	candidate := differentialKafkaProxyObservationForTest(t, differentialKafkaProxyCandidateCallsForTest(spec))
	oracle := differentialKafkaProxyObservationForTest(t, differentialKafkaProxyOracleCallsForTest(spec))
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialKafkaProxyPubSubRecord(
		spec, candidate, oracle, differentialKafkaProxyNormalizationPolicyForTest(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare = passed %t, diff %q, err %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("comparator mutated input observations")
	}
}

func TestCompareDifferentialKafkaProxyPubSubRecordRejectsSemanticMutations(t *testing.T) {
	spec := differentialKafkaProxyCases()[0]
	tests := []struct {
		name   string
		mutate func(*DifferentialObservation)
	}{
		{name: "missing websocket header", mutate: func(observation *DifferentialObservation) {
			delete(observation.Headers, "Upgrade")
		}},
		{name: "upstream projection", mutate: func(observation *DifferentialObservation) {
			observation.Upstream.Method = "KAFKA"
		}},
		{name: "missing broker call", mutate: func(observation *DifferentialObservation) {
			observation.UpstreamCalls = observation.UpstreamCalls[:3]
		}},
		{name: "wrong list offset", mutate: func(observation *DifferentialObservation) {
			mutateDifferentialKafkaProxyTranscript(t, observation, func(transcript *differentialKafkaProxyTranscript) {
				transcript.ListOffset.Offset++
			})
		}},
		{name: "wrong fetch sequence", mutate: func(observation *DifferentialObservation) {
			mutateDifferentialKafkaProxyTranscript(t, observation, func(transcript *differentialKafkaProxyTranscript) {
				transcript.Fetch.Sequence++
			})
		}},
		{name: "missing record", mutate: func(observation *DifferentialObservation) {
			mutateDifferentialKafkaProxyTranscript(t, observation, func(transcript *differentialKafkaProxyTranscript) {
				transcript.Fetch.Messages = nil
			})
		}},
		{name: "wrong record value", mutate: func(observation *DifferentialObservation) {
			mutateDifferentialKafkaProxyTranscript(t, observation, func(transcript *differentialKafkaProxyTranscript) {
				transcript.Fetch.Messages[0].Value = "wrong"
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := differentialKafkaProxyObservationForTest(
				t, differentialKafkaProxyCandidateCallsForTest(spec),
			)
			test.mutate(&candidate)
			_, _, err := compareDifferentialKafkaProxyPubSubRecord(
				spec, candidate,
				differentialKafkaProxyObservationForTest(t, differentialKafkaProxyOracleCallsForTest(spec)),
				differentialKafkaProxyNormalizationPolicyForTest(),
			)
			if err == nil {
				t.Fatal("compare error = nil, want semantic mutation rejection")
			}
		})
	}
}

func differentialKafkaProxyNormalizationPolicyForTest() NormalizationPolicy {
	return NormalizationPolicy{
		SchemaVersion: 1,
		Headers: HeaderNormalizationPolicy{
			CanonicalizeNames:        true,
			StripHopByHop:            true,
			ContentLengthIfBodyEqual: true,
		},
		FixtureEndpointMapping: true,
	}
}

func differentialKafkaProxyObservationForTest(
	t *testing.T,
	brokerCalls []DifferentialUpstreamObservation,
) DifferentialObservation {
	t.Helper()
	observation, err := newDifferentialKafkaProxyObservation(
		differentialKafkaProxyCases()[0],
		kafka_proxy.PubSubResponse{
			Sequence: 3, Kind: kafka_proxy.RespKafkaListOffset,
			Offset: differentialKafkaPubSubHighWatermark,
		},
		kafka_proxy.PubSubResponse{
			Sequence: 6, Kind: kafka_proxy.RespKafkaFetch,
			Messages: []kafka_proxy.KafkaMessage{{
				Offset:    differentialKafkaPubSubRecordOffset,
				Timestamp: differentialKafkaPubSubRecordTimestamp,
				Key:       []byte(differentialKafkaPubSubRecordKey),
				Value:     []byte(differentialKafkaPubSubRecordValue),
			}},
		},
	)
	if err != nil {
		t.Fatalf("build observation: %v", err)
	}
	observation.UpstreamFixture = differentialKafkaProxyCases()[0].Fixture.Name
	observation.UpstreamAddress = "127.0.0.1:19092"
	observation.UpstreamCalls = brokerCalls
	observation.Upstream = observation.UpstreamCalls[len(observation.UpstreamCalls)-1]
	return observation
}

func differentialKafkaProxyCandidateCallsForTest(spec DifferentialCase) []DifferentialUpstreamObservation {
	return []DifferentialUpstreamObservation{
		{
			Received: true, Fixture: spec.Fixture.Name, Method: differentialKafkaListOffsetsMethod,
			Path: differentialKafkaPubSubTopic, Host: "0", Body: "-1",
		},
		{
			Received: true, Fixture: spec.Fixture.Name, Method: differentialKafkaListOffsetsMethod,
			Path: differentialKafkaPubSubTopic, Host: "0", Body: "-2",
		},
		{
			Received: true, Fixture: spec.Fixture.Name, Method: differentialKafkaListOffsetsMethod,
			Path: differentialKafkaPubSubTopic, Host: "0", Body: "-1",
		},
		{
			Received: true, Fixture: spec.Fixture.Name, Method: differentialKafkaFetchMethod,
			Path: differentialKafkaPubSubTopic, Host: "0", Body: "14",
		},
	}
}

func differentialKafkaProxyOracleCallsForTest(spec DifferentialCase) []DifferentialUpstreamObservation {
	return []DifferentialUpstreamObservation{
		{
			Received: true, Fixture: spec.Fixture.Name, Method: differentialKafkaListOffsetsMethod,
			Path: differentialKafkaPubSubTopic, Host: "0", Body: "-1",
		},
		{
			Received: true, Fixture: spec.Fixture.Name, Method: differentialKafkaFetchMethod,
			Path: differentialKafkaPubSubTopic, Host: "0", Body: "14",
		},
	}
}

func mutateDifferentialKafkaProxyTranscript(
	t *testing.T,
	observation *DifferentialObservation,
	mutate func(*differentialKafkaProxyTranscript),
) {
	t.Helper()
	transcript, err := decodeDifferentialKafkaProxyTranscript(observation.Body)
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
