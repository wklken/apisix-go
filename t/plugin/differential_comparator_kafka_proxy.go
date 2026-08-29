package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
)

func init() {
	differentialComparatorRegistry[differentialKafkaProxyPubSubPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"kafka-proxy": {}},
		compare:        compareDifferentialKafkaProxyPubSubRecord,
	}
}

func compareDifferentialKafkaProxyPubSubRecord(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialKafkaProxyCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned kafka-proxy case",
			spec.ComparisonPolicy,
		)
	}
	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		if err := normalizeDifferentialKafkaProxyObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialKafkaProxyObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	wantHeaders := map[string][]string{
		"Connection": {"upgrade"},
		"Upgrade":    {"websocket"},
	}
	if len(observation.Steps) != 0 || observation.Status != http.StatusSwitchingProtocols ||
		!reflect.DeepEqual(observation.Headers, wantHeaders) ||
		observation.Host != spec.Request.Host || observation.SNI != spec.Request.SNI ||
		observation.SecurityDecision != spec.SecurityDecision || observation.RetryCount != 0 ||
		len(observation.RouteObserver) != 0 || observation.UpstreamFixture != spec.Fixture.Name ||
		observation.UpstreamAddress == "" ||
		observation.File != nil {
		return fmt.Errorf("%s kafka-proxy WebSocket observation envelope is not exact", side)
	}
	// kafka-go's ReadLastOffset probes both bounds while APISIX's basic
	// consumer sends one ListOffsets request. Validate each native sequence
	// exactly, then compare the two logical PubSub commands.
	var wantCalls []DifferentialUpstreamObservation
	switch side {
	case "candidate":
		wantCalls = differentialKafkaProxyExpectedCandidateBrokerCalls(spec)
	case "oracle":
		wantCalls = differentialKafkaProxyExpectedOracleBrokerCalls(spec)
	default:
		return fmt.Errorf("unknown kafka-proxy observation side %q", side)
	}
	if !reflect.DeepEqual(observation.UpstreamCalls, wantCalls) ||
		!reflect.DeepEqual(observation.Upstream, wantCalls[len(wantCalls)-1]) {
		return fmt.Errorf("%s kafka-proxy native broker calls = %#v", side, observation.UpstreamCalls)
	}
	transcript, err := decodeDifferentialKafkaProxyTranscript(observation.Body)
	if err != nil {
		return fmt.Errorf("%s kafka-proxy transcript: %w", side, err)
	}
	if transcript.ListOffset.Sequence != 3 ||
		transcript.ListOffset.Offset != differentialKafkaPubSubHighWatermark {
		return fmt.Errorf("%s kafka-proxy list-offset response = %#v", side, transcript.ListOffset)
	}
	if transcript.Fetch.Sequence != 6 || len(transcript.Fetch.Messages) != 1 {
		return fmt.Errorf("%s kafka-proxy fetch response = %#v", side, transcript.Fetch)
	}
	message := transcript.Fetch.Messages[0]
	if message.Offset != differentialKafkaPubSubRecordOffset ||
		message.Timestamp != differentialKafkaPubSubRecordTimestamp ||
		message.Key != differentialKafkaPubSubRecordKey ||
		message.Value != differentialKafkaPubSubRecordValue {
		return fmt.Errorf("%s kafka-proxy fetched record = %#v", side, message)
	}
	canonical, err := json.Marshal(transcript)
	if err != nil {
		return fmt.Errorf("marshal canonical Kafka PubSub transcript: %w", err)
	}
	observation.Body = string(canonical)
	observation.UpstreamCalls = differentialKafkaProxyExpectedOracleBrokerCalls(spec)
	observation.Upstream = observation.UpstreamCalls[len(observation.UpstreamCalls)-1]
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	return nil
}

func differentialKafkaProxyExpectedCandidateBrokerCalls(spec DifferentialCase) []DifferentialUpstreamObservation {
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

func differentialKafkaProxyExpectedOracleBrokerCalls(spec DifferentialCase) []DifferentialUpstreamObservation {
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
