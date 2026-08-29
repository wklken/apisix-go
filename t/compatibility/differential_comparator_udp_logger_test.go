package pluginintegration

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialUDPLoggerFixtureDeliveryAcceptsOneExactDatagram(t *testing.T) {
	spec := differentialCasesForPlugin("udp-logger")[0]
	candidate, oracle := differentialUDPLoggerComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialUDPLoggerFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned UDP logger observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("UDP logger comparison mutated caller observations")
	}
}

func TestCompareDifferentialUDPLoggerFixtureDeliveryRejectsBoundaryAndPayloadDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "second datagram",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls = append(oracle.UpstreamCalls, oracle.UpstreamCalls[0])
			},
			want: "exactly 2",
		},
		{
			name: "invalid timestamp",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(candidate, differentialUDPLoggerFixtureMethod)
				raw.Body = strings.Replace(raw.Body, "2026-08-29T00:00:00+08:00", "not-time", 1)
				candidate.Upstream = *raw
			},
			want: "@timestamp",
		},
		{
			name: "numeric timestamp",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(oracle, differentialUDPLoggerFixtureMethod)
				raw.Body = strings.Replace(raw.Body, `"@timestamp":"2026-08-28T16:00:01Z"`, `"@timestamp":1700000000`, 1)
				oracle.Upstream = *raw
			},
			want: "@timestamp is not a string",
		},
		{
			name: "extra field",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(candidate, differentialUDPLoggerFixtureMethod)
				raw.Body = strings.Replace(raw.Body, "}", `,"extra":"value"}`, 1)
				candidate.Upstream = *raw
			},
			want: "unknown field",
		},
		{
			name: "empty service id mirrors candidate instead of oracle",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(candidate, differentialUDPLoggerFixtureMethod)
				raw.Body = strings.Replace(raw.Body, "}", `,"service_id":""}`, 1)
				candidate.Upstream = *raw
			},
			want: "unknown field",
		},
		{
			name: "wrong client ip",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(oracle, differentialUDPLoggerFixtureMethod)
				raw.Body = strings.Replace(raw.Body, "127.0.0.1", "192.0.2.2", 1)
				oracle.Upstream = *raw
			},
			want: "client_ip",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("udp-logger")[0]
			candidate, oracle := differentialUDPLoggerComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)
			passed, _, err := compareDifferentialUDPLoggerFixtureDelivery(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare malformed UDP logger contract = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialUDPLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	return differentialRawLoggerComparatorObservations(
		spec,
		differentialUDPLoggerFixtureMethod,
		`{"@timestamp":"2026-08-29T00:00:00+08:00","case name":"logger format in plugin","client_ip":"127.0.0.1","host":"localhost","route_id":"`+spec.RouteID+`"}`,
		`{"route_id":"`+spec.RouteID+`","host":"localhost","client_ip":"127.0.0.1","case name":"logger format in plugin","@timestamp":"2026-08-28T16:00:01Z"}`,
	)
}
