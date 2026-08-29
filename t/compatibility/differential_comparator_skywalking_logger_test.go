package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialSkyWalkingLoggerFixtureDeliveryAcceptsExactSemanticEntry(t *testing.T) {
	spec := differentialCasesForPlugin("skywalking-logger")[0]
	candidate, oracle := differentialSkyWalkingLoggerComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialSkyWalkingLoggerFixtureDelivery(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned SkyWalking observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("SkyWalking comparison mutated caller observations")
	}
}

func TestCompareDifferentialSkyWalkingLoggerFixtureDeliveryRejectsLooseContracts(t *testing.T) {
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
			want: "pinned",
		},
		{
			name: "wrong content type",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialLoggerTestCall(candidate, differentialSkyWalkingLoggerPath)
				call.Headers["Content-Type"] = []string{"application/x-json"}
				candidate.Upstream = *call
			},
			want: "Content-Type",
		},
		{
			name: "second entry",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialLoggerTestCall(oracle, differentialSkyWalkingLoggerPath)
				call.Body = strings.TrimSuffix(call.Body, "]") + ",{}]"
			},
			want: "exactly one entry",
		},
		{
			name: "wrong service",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialLoggerTestCall(candidate, differentialSkyWalkingLoggerPath)
				call.Body = strings.Replace(call.Body, `"service":"APISIX"`, `"service":"other"`, 1)
				candidate.Upstream = *call
			},
			want: "service",
		},
		{
			name: "wrong service instance",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialLoggerTestCall(oracle, differentialSkyWalkingLoggerPath)
				call.Body = strings.Replace(
					call.Body,
					`"serviceInstance":"APISIX Instance Name"`,
					`"serviceInstance":"other"`,
					1,
				)
			},
			want: "serviceInstance",
		},
		{
			name: "wrong endpoint",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialLoggerTestCall(candidate, differentialSkyWalkingLoggerPath)
				call.Body = strings.Replace(call.Body, specEndpointJSON(), `"endpoint":"/other"`, 1)
				candidate.Upstream = *call
			},
			want: "endpoint",
		},
		{
			name: "extra payload field",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialLoggerTestCall(oracle, differentialSkyWalkingLoggerPath)
				call.Body = strings.Replace(
					call.Body,
					`{\"route_id\":\"differential-skywalking-logger-delivery\",\"my_ip\":\"127.0.0.1\"}`,
					`{\"route_id\":\"differential-skywalking-logger-delivery\",\"my_ip\":\"127.0.0.1\",\"time\":1700000000}`,
					1,
				)
			},
			want: "unknown field",
		},
		{
			name: "unexpected trace context",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialLoggerTestCall(candidate, differentialSkyWalkingLoggerPath)
				call.Body = strings.Replace(
					call.Body,
					`"body":`,
					`"traceContext":{"traceId":"dynamic","traceSegmentId":"dynamic","spanId":1},"body":`,
					1,
				)
				candidate.Upstream = *call
			},
			want: "unknown field",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("skywalking-logger")[0]
			candidate, oracle := differentialSkyWalkingLoggerComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)
			passed, _, err := compareDifferentialSkyWalkingLoggerFixtureDelivery(
				spec,
				candidate,
				oracle,
				testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare malformed SkyWalking contract = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialSkyWalkingLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	candidateCall := DifferentialUpstreamObservation{
		Received: true,
		Fixture:  spec.Fixture.Name,
		Method:   http.MethodPost,
		Path:     differentialSkyWalkingLoggerPath,
		Headers:  map[string][]string{"Content-Type": {"application/json"}},
		Body: `[{"body":{"json":{"json":"{\"my_ip\":\"127.0.0.1\",\"route_id\":\"` + spec.RouteID +
			`\"}"}},"service":"APISIX","serviceInstance":"APISIX Instance Name","endpoint":"` +
			spec.Steps[0].Request.Path + `"}]`,
	}
	candidate, oracle := differentialLoggerComparatorPair(
		spec,
		"127.0.0.1:31007",
		"host.containers.internal:1980",
		candidateCall,
	)
	oracleCall := differentialLoggerTestCall(&oracle, differentialSkyWalkingLoggerPath)
	oracleCall.Host = "host.containers.internal"
	oracleCall.Body = `[{"endpoint":"` + spec.Steps[0].Request.Path +
		`","serviceInstance":"APISIX Instance Name","service":"APISIX","body":{"json":{"json":"{\"route_id\":\"` +
		spec.RouteID + `\",\"my_ip\":\"127.0.0.1\"}"}}}]`
	oracle.Upstream = oracle.UpstreamCalls[len(oracle.UpstreamCalls)-1]
	return candidate, oracle
}

func specEndpointJSON() string {
	return `"endpoint":"/logger/skywalking"`
}
