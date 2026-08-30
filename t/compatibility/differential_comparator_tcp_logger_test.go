package pluginintegration

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialTCPLoggerFixtureDeliveryAcceptsOneExactFrame(t *testing.T) {
	spec := differentialCasesForPlugin("tcp-logger")[0]
	candidate, oracle := differentialRawLoggerComparatorObservations(
		spec,
		differentialTCPLoggerFixtureMethod,
		`{"case name":"logger format in plugin","vip":"127.0.0.1","status":200,"route_id":"`+spec.RouteID+`"}`,
		`{"route_id":"`+spec.RouteID+`","status":200,"vip":"127.0.0.1","case name":"logger format in plugin"}`,
	)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialTCPLoggerFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned TCP logger observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("TCP logger comparison mutated caller observations")
	}
}

func TestCompareDifferentialTCPLoggerFixtureDeliveryRejectsBoundaryAndPayloadDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned case",
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Fixture.WireProtocol = "tcp"
			},
			want: "pinned",
		},
		{
			name: "two frames",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls = append(candidate.UpstreamCalls, candidate.UpstreamCalls[1])
			},
			want: "exactly 2",
		},
		{
			name: "array instead of single object",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(oracle, differentialTCPLoggerFixtureMethod)
				raw.Body = "[" + raw.Body + "]"
				oracle.Upstream = *raw
			},
			want: "top-level value is not an object",
		},
		{
			name: "string status",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(candidate, differentialTCPLoggerFixtureMethod)
				raw.Body = strings.Replace(raw.Body, `"status":200`, `"status":"200"`, 1)
				candidate.Upstream = *raw
			},
			want: "status is not a number",
		},
		{
			name: "stale service survives",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(oracle, differentialTCPLoggerFixtureMethod)
				raw.Body = strings.Replace(raw.Body, "}", `,"service_id":"stale-service"}`, 1)
				oracle.Upstream = *raw
			},
			want: "unknown field",
		},
		{
			name: "wrong vip",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				raw := differentialRawLoggerTestCall(candidate, differentialTCPLoggerFixtureMethod)
				raw.Body = strings.Replace(raw.Body, "127.0.0.1", "192.0.2.1", 1)
				candidate.Upstream = *raw
			},
			want: "vip",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("tcp-logger")[0]
			candidate, oracle := differentialRawLoggerComparatorObservations(
				spec,
				differentialTCPLoggerFixtureMethod,
				`{"case name":"logger format in plugin","vip":"127.0.0.1","status":200,"route_id":"`+spec.RouteID+`"}`,
				`{"route_id":"`+spec.RouteID+`","status":200,"vip":"127.0.0.1","case name":"logger format in plugin"}`,
			)
			test.mutate(&spec, &candidate, &oracle)
			passed, _, err := compareDifferentialTCPLoggerFixtureDelivery(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare malformed TCP logger contract = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialRawLoggerComparatorObservations(
	spec DifferentialCase,
	rawMethod string,
	candidateBody string,
	oracleBody string,
) (DifferentialObservation, DifferentialObservation) {
	origin := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: spec.Steps[0].Request.Method,
		Path: spec.Steps[0].Request.Path, Host: "differential.example.test",
	}
	raw := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: rawMethod, Body: candidateBody,
	}
	candidate := DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status: spec.Fixture.Response.Status, Body: spec.Fixture.Response.Body,
			Headers: differentialNetworkLoggerCandidateGatewayHeaders(
				len(spec.Fixture.Response.Body), "text/plain; charset=utf-8",
			),
			Host: spec.Steps[0].Request.Host, SecurityDecision: spec.Steps[0].SecurityDecision,
		}},
		UpstreamFixture: spec.Fixture.Name, UpstreamAddress: "127.0.0.1:31009",
		UpstreamCalls: []DifferentialUpstreamObservation{origin, raw}, Upstream: raw,
	}
	oracle := copyDifferentialObservation(candidate)
	oracle.Steps[0].Headers = differentialNetworkLoggerOracleGatewayHeaders(
		len(spec.Fixture.Response.Body), "text/plain; charset=utf-8",
	)
	oracle.UpstreamAddress = "host.containers.internal:1980"
	oracle.UpstreamCalls[1].Body = oracleBody
	oracle.UpstreamCalls[0], oracle.UpstreamCalls[1] = oracle.UpstreamCalls[1], oracle.UpstreamCalls[0]
	oracle.Upstream = oracle.UpstreamCalls[0]
	return candidate, oracle
}

func differentialRawLoggerTestCall(
	observation *DifferentialObservation,
	method string,
) *DifferentialUpstreamObservation {
	for index := range observation.UpstreamCalls {
		if observation.UpstreamCalls[index].Method == method {
			return &observation.UpstreamCalls[index]
		}
	}
	panic("missing differential raw logger call " + method)
}
