package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialSyslogTCPDeliveryNormalizesOnlyTimestampPIDAndAddress(t *testing.T) {
	spec := differentialSyslogCases()[0]
	candidate, oracle := differentialSyslogComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialSyslogTCPDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned syslog observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("syslog comparison mutated caller observations")
	}
}

func TestCompareDifferentialSyslogTCPDeliveryRejectsLooseSemantics(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned case",
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Fixture.ExpectedCalls++
			},
			want: "pinned syslog case",
		},
		{
			name: "wrong gateway response",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[0].Status = http.StatusCreated
			},
			want: "gateway step",
		},
		{
			name: "missing raw frame",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls = candidate.UpstreamCalls[:1]
				candidate.Upstream = candidate.UpstreamCalls[0]
			},
			want: "exactly 2",
		},
		{
			name: "raw call has HTTP metadata",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialSyslogRawTestCall(candidate)
				call.Method = http.MethodPost
				candidate.Upstream = *call
			},
			want: "raw TCP call",
		},
		{
			name: "raw call is missing TCP protocol marker",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialSyslogRawTestCall(candidate)
				call.Method = ""
				candidate.Upstream = *call
			},
			want: "raw TCP call",
		},
		{
			name: "gateway content type drift",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Steps[0].Headers = nil
			},
			want: "gateway headers",
		},
		{
			name: "non-millisecond timestamp",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialSyslogRawTestCall(oracle)
				call.Body = strings.Replace(call.Body, ".789Z", ".789123Z", 1)
				oracle.Upstream = *call
			},
			want: "millisecond UTC timestamp",
		},
		{
			name: "process hostname is not request host",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialSyslogRawTestCall(candidate)
				call.Body = strings.Replace(call.Body, " gateway.example.test apisix ", " candidate-host apisix ", 1)
				candidate.Upstream = *call
			},
			want: "hostname",
		},
		{
			name: "wrong RFC5424 priority",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialSyslogRawTestCall(oracle)
				call.Body = strings.Replace(call.Body, "<46>1", "<14>1", 1)
				oracle.Upstream = *call
			},
			want: "RFC5424",
		},
		{
			name: "extra JSON field",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialSyslogRawTestCall(candidate)
				call.Body = strings.Replace(call.Body, `"route_id":"differential-syslog-tcp"}`, `"route_id":"differential-syslog-tcp","extra":true}`, 1)
				candidate.Upstream = *call
			},
			want: "field",
		},
		{
			name: "second frame",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialSyslogRawTestCall(oracle)
				call.Body += call.Body
				oracle.Upstream = *call
			},
			want: "single newline-terminated",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialSyslogCases()[0]
			candidate, oracle := differentialSyslogComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialSyslogTCPDelivery(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose syslog semantics = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialSyslogComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	origin := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodGet,
		Path: differentialSyslogGatewayPath, Host: "differential.example.test",
	}
	candidateRaw := DifferentialUpstreamObservation{
		Received: true,
		Fixture:  spec.Fixture.Name,
		Method:   "TCP",
		Body:     `<46>1 2026-08-29T01:02:03.456Z gateway.example.test apisix 1234 - - {"case":"syslog","route_id":"` + spec.RouteID + `"}` + "\n",
	}
	oracleRaw := candidateRaw
	oracleRaw.Body = strings.Replace(oracleRaw.Body, "01:02:03.456Z", "01:02:04.789Z", 1)
	oracleRaw.Body = strings.Replace(oracleRaw.Body, " apisix 1234 ", " apisix 4321 ", 1)
	candidate := differentialSyslogObservation(
		spec,
		"127.0.0.1:31051",
		[]DifferentialUpstreamObservation{candidateRaw, origin},
	)
	oracle := differentialSyslogObservation(
		spec,
		"host.containers.internal:1980",
		[]DifferentialUpstreamObservation{origin, oracleRaw},
	)
	oracle.Steps[0].Headers = differentialNetworkLoggerOracleGatewayHeaders(
		len(spec.Fixture.Response.Body), "text/plain; charset=utf-8",
	)
	return candidate, oracle
}

func differentialSyslogObservation(
	spec DifferentialCase,
	address string,
	calls []DifferentialUpstreamObservation,
) DifferentialObservation {
	return DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status: spec.Fixture.Response.Status, Body: spec.Fixture.Response.Body,
			Headers: differentialNetworkLoggerCandidateGatewayHeaders(
				len(spec.Fixture.Response.Body), "text/plain; charset=utf-8",
			),
			Host: spec.Steps[0].Request.Host, SNI: spec.Steps[0].Request.SNI,
			SecurityDecision: spec.Steps[0].SecurityDecision,
		}},
		UpstreamFixture: spec.Fixture.Name,
		UpstreamAddress: address,
		Upstream:        calls[0],
		UpstreamCalls:   calls,
	}
}

func differentialSyslogRawTestCall(observation *DifferentialObservation) *DifferentialUpstreamObservation {
	for index := range observation.UpstreamCalls {
		if observation.UpstreamCalls[index].Method == "TCP" {
			return &observation.UpstreamCalls[index]
		}
	}
	panic("raw syslog fixture call not found")
}
