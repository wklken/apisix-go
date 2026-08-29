package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialSLSLoggerTLSDeliveryNormalizesOnlyTimestampPIDAndAddress(t *testing.T) {
	spec := differentialSLSLoggerCases()[0]
	candidate, oracle := differentialSLSLoggerComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialSLSLoggerTLSDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned sls-logger observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("sls-logger comparison mutated caller observations")
	}
}

func TestCompareDifferentialSLSLoggerTLSDeliveryRejectsLooseSemantics(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned case",
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Config["unexpected"] = true
			},
			want: "pinned sls-logger case",
		},
		{
			name: "missing TLS frame",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls = nil
				candidate.Upstream = DifferentialUpstreamObservation{}
			},
			want: "exactly 1",
		},
		{
			name: "raw call has HTTP metadata",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls[0].Method = http.MethodPost
				oracle.Upstream = oracle.UpstreamCalls[0]
			},
			want: "raw TLS TCP call",
		},
		{
			name: "raw call is missing TCP protocol marker",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[0].Method = ""
				candidate.Upstream = candidate.UpstreamCalls[0]
			},
			want: "raw TLS TCP call",
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
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[0].Body = strings.Replace(candidate.UpstreamCalls[0].Body, ".456Z", ".456789Z", 1)
				candidate.Upstream = candidate.UpstreamCalls[0]
			},
			want: "millisecond UTC timestamp",
		},
		{
			name: "process hostname is not request host",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[0].Body = strings.Replace(candidate.UpstreamCalls[0].Body, " gateway.example.test apisix ", " candidate-host apisix ", 1)
				candidate.Upstream = candidate.UpstreamCalls[0]
			},
			want: "hostname",
		},
		{
			name: "wrong project",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls[0].Body = strings.Replace(oracle.UpstreamCalls[0].Body, `project="differential-project"`, `project="other"`, 1)
				oracle.Upstream = oracle.UpstreamCalls[0]
			},
			want: "structured data",
		},
		{
			name: "wrong access key secret",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[0].Body = strings.Replace(candidate.UpstreamCalls[0].Body, `access-key-secret="differential-access-key-secret"`, `access-key-secret="other"`, 1)
				candidate.Upstream = candidate.UpstreamCalls[0]
			},
			want: "structured data",
		},
		{
			name: "extra JSON field",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls[0].Body = strings.Replace(oracle.UpstreamCalls[0].Body, `"route_id":"differential-sls-logger-tls"}`, `"route_id":"differential-sls-logger-tls","extra":true}`, 1)
				oracle.Upstream = oracle.UpstreamCalls[0]
			},
			want: "field",
		},
		{
			name: "second frame",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[0].Body += candidate.UpstreamCalls[0].Body
				candidate.Upstream = candidate.UpstreamCalls[0]
			},
			want: "single newline-terminated",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialSLSLoggerCases()[0]
			candidate, oracle := differentialSLSLoggerComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialSLSLoggerTLSDelivery(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose sls-logger semantics = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialSLSLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	candidateFrame := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: "TCP",
		Body: `<46>1 2026-08-29T01:02:03.456Z gateway.example.test apisix 1234 - ` +
			`[logservice project="differential-project" logstore="differential-logstore" access-key-id="differential-access-key-id" access-key-secret="differential-access-key-secret"] ` +
			`{"case":"sls-logger","route_id":"` + spec.RouteID + `"}` + "\n",
	}
	oracleFrame := candidateFrame
	oracleFrame.Body = strings.Replace(oracleFrame.Body, "01:02:03.456Z", "01:02:04.789Z", 1)
	oracleFrame.Body = strings.Replace(oracleFrame.Body, " apisix 1234 ", " apisix 4321 ", 1)
	candidate := differentialSLSLoggerObservation(spec, "127.0.0.1:31052", candidateFrame)
	oracle := differentialSLSLoggerObservation(spec, "host.containers.internal:1980", oracleFrame)
	oracle.Steps[0].Headers = differentialNetworkLoggerOracleGatewayHeaders(
		len(spec.Fixture.Response.Body), "text/plain; charset=utf-8",
	)
	delete(oracle.Steps[0].Headers, "Content-Length")
	oracle.Steps[0].Headers["Date"] = []string{"Fri, 28 Aug 2026 17:50:03 GMT"}
	return candidate, oracle
}

func differentialSLSLoggerObservation(
	spec DifferentialCase,
	address string,
	frame DifferentialUpstreamObservation,
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
		Upstream:        frame,
		UpstreamCalls:   []DifferentialUpstreamObservation{frame},
	}
}
