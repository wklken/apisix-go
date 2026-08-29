package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDifferentialRocketMQLoggerComparatorAcceptsExactPublish(t *testing.T) {
	spec := differentialRocketMQLoggerCases()[0]
	candidate := differentialRocketMQLoggerObservationForTest(spec, "candidate")
	oracle := differentialRocketMQLoggerObservationForTest(spec, "oracle")
	equal, diff, err := compareDifferentialRocketMQLoggerPublish(
		spec, candidate, oracle, differentialRocketMQNormalizationPolicyForTest(),
	)
	if err != nil || !equal || diff != "" {
		t.Fatalf("compare = %v, %q, %v", equal, diff, err)
	}
}

func TestDifferentialRocketMQLoggerComparatorRejectsWrongEnvelopeOrPayload(t *testing.T) {
	spec := differentialRocketMQLoggerCases()[0]
	tests := []struct {
		name   string
		mutate func(*DifferentialObservation)
		want   string
	}{
		{
			name: "wrong tag",
			mutate: func(observation *DifferentialObservation) {
				observation.UpstreamCalls[1].Headers[differentialRocketMQTagHeader] = []string{"wrong"}
				observation.Upstream = observation.UpstreamCalls[1]
			},
			want: "topic/key/tag/queue envelope",
		},
		{
			name: "extra log field",
			mutate: func(observation *DifferentialObservation) {
				observation.UpstreamCalls[1].Body = `{"route_id":"differential-rocketmq-logger-format","x_ip":"127.0.0.1","extra":true}`
				observation.Upstream = observation.UpstreamCalls[1]
			},
			want: "field count",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := differentialRocketMQLoggerObservationForTest(spec, "candidate")
			test.mutate(&candidate)
			_, _, err := compareDifferentialRocketMQLoggerPublish(
				spec,
				candidate,
				differentialRocketMQLoggerObservationForTest(spec, "oracle"),
				differentialRocketMQNormalizationPolicyForTest(),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func differentialRocketMQLoggerObservationForTest(
	spec DifferentialCase,
	side string,
) DifferentialObservation {
	headers := map[string][]string{
		"Content-Length": {"12"},
		"Content-Type":   {"text/plain; charset=utf-8"},
	}
	if side == "candidate" {
		headers["Date"] = []string{time.Now().UTC().Format(http.TimeFormat)}
		headers["Server"] = []string{"APISIX/apisix-go"}
	} else {
		headers["Server"] = []string{"APISIX/3.17.0"}
	}
	origin := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name,
		Method: http.MethodGet, Path: "/hello", Host: "differential.example.test",
	}
	message := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name,
		Method: differentialRocketMQMethod, Path: "test2", Host: "key1",
		Headers: map[string][]string{
			differentialRocketMQTagHeader:     {"tag1"},
			differentialRocketMQQueueIDHeader: {"0"},
		},
		Body: `{"route_id":"differential-rocketmq-logger-format","x_ip":"127.0.0.1"}`,
	}
	return DifferentialObservation{
		Status: spec.Fixture.Response.Status, Headers: headers, Body: spec.Fixture.Response.Body,
		UpstreamFixture: spec.Fixture.Name, UpstreamAddress: "127.0.0.1:19876",
		Host: spec.Request.Host, SecurityDecision: spec.SecurityDecision,
		Upstream: message, UpstreamCalls: []DifferentialUpstreamObservation{origin, message},
	}
}

func differentialRocketMQNormalizationPolicyForTest() NormalizationPolicy {
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
