package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialDatadogRequiresSixOrderedSingleMetricDatagrams(t *testing.T) {
	spec := differentialDatadogCases()[0]
	candidate, oracle := differentialDatadogComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialDatadogSixDatagrams(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned Datadog observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("Datadog comparison mutated caller observations")
	}
}

func TestCompareDifferentialDatadogRejectsMergedOrLooseDatagrams(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned case",
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Fixture.ExpectedCalls = 1
			},
			want: "pinned",
		},
		{
			name: "one merged datagram",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				merged := candidate.UpstreamCalls[0]
				bodies := make([]string, 0, len(candidate.UpstreamCalls))
				for _, call := range candidate.UpstreamCalls {
					bodies = append(bodies, call.Body)
				}
				merged.Body = strings.Join(bodies, "\n")
				candidate.UpstreamCalls = []DifferentialUpstreamObservation{merged}
				candidate.Upstream = merged
			},
			want: "exactly 6",
		},
		{
			name: "two metrics in one of six datagrams",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[0].Body += "\n" + candidate.UpstreamCalls[1].Body
			},
			want: "one metric",
		},
		{
			name: "metric datagram order",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls[1], oracle.UpstreamCalls[2] = oracle.UpstreamCalls[2], oracle.UpstreamCalls[1]
			},
			want: "datagram 2",
		},
		{
			name: "UDP method",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[3].Method = "TCP"
			},
			want: "UDP",
		},
		{
			name: "counter value",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls[0].Body = strings.Replace(oracle.UpstreamCalls[0].Body, ":1|c", ":2|c", 1)
			},
			want: "request.counter",
		},
		{
			name: "metric type",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[1].Body = strings.Replace(candidate.UpstreamCalls[1].Body, "|h|#", "|ms|#", 1)
			},
			want: "request.latency",
		},
		{
			name: "negative latency",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[2].Body = strings.Replace(candidate.UpstreamCalls[2].Body, ":3|h", ":-1|h", 1)
			},
			want: "nonnegative decimal",
		},
		{
			name: "candidate ingress wire size",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[4].Body = strings.Replace(candidate.UpstreamCalls[4].Body, ":89|ms", ":88|ms", 1)
			},
			want: "ingress.size",
		},
		{
			name: "egress size",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls[5].Body = strings.Replace(oracle.UpstreamCalls[5].Body, ":133|ms", ":132|ms", 1)
				oracle.Upstream = oracle.UpstreamCalls[5]
			},
			want: "egress.size",
		},
		{
			name: "route tag",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[4].Body = strings.Replace(candidate.UpstreamCalls[4].Body, ",route_name:datadog", "", 1)
			},
			want: "tags",
		},
		{
			name: "balancer IP",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls[3].Body = strings.Replace(oracle.UpstreamCalls[3].Body, "balancer_ip:192.0.2.10", "balancer_ip:not-an-ip", 1)
			},
			want: "balancer_ip",
		},
		{
			name: "gateway body",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[0].Body = "wrong"
			},
			want: "gateway step",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialDatadogCases()[0]
			candidate, oracle := differentialDatadogComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialDatadogSixDatagrams(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose Datadog contract = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialDatadogComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	candidateCalls := differentialDatadogMetricCalls(spec, "127.0.0.1", []string{"1", "2", "3", "1", "89", "164"})
	candidate := differentialDatadogObservation(spec, "127.0.0.1:31170", candidateCalls)
	oracleCalls := differentialDatadogMetricCalls(
		spec,
		"192.0.2.10",
		[]string{"1", "1.25", "3.5", "2.25", "108", "133"},
	)
	oracle := differentialDatadogObservation(spec, "host.containers.internal:1980", oracleCalls)
	oracle.Steps[0].Headers = differentialNetworkLoggerOracleGatewayHeaders(
		len(spec.Fixture.Response.Body), "text/plain; charset=utf-8",
	)
	oracle.Upstream = oracleCalls[2]
	return candidate, oracle
}

func differentialDatadogObservation(
	spec DifferentialCase,
	address string,
	calls []DifferentialUpstreamObservation,
) DifferentialObservation {
	return DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status: http.StatusOK,
			Headers: differentialNetworkLoggerCandidateGatewayHeaders(
				len(spec.Fixture.Response.Body), "text/plain",
			),
			Body: "opentracing", Host: spec.Steps[0].Request.Host,
			SecurityDecision: spec.Steps[0].SecurityDecision,
		}},
		UpstreamFixture: spec.Fixture.Name, UpstreamAddress: address,
		UpstreamCalls: calls, Upstream: calls[len(calls)-1],
	}
}

func differentialDatadogMetricCalls(
	spec DifferentialCase,
	balancerIP string,
	values []string,
) []DifferentialUpstreamObservation {
	names := []string{
		"request.counter", "request.latency", "upstream.latency",
		"apisix.latency", "ingress.size", "egress.size",
	}
	types := []string{"c", "h", "h", "h", "ms", "ms"}
	tags := "source:apisix,route_name:datadog,balancer_ip:" + balancerIP +
		",response_status:200,response_status_class:2xx,scheme:http"
	calls := make([]DifferentialUpstreamObservation, 0, len(names))
	for index, name := range names {
		calls = append(calls, DifferentialUpstreamObservation{
			Received: true, Fixture: spec.Fixture.Name, Method: "UDP",
			Body: "apisix." + name + ":" + values[index] + "|" + types[index] + "|#" + tags,
		})
	}
	return calls
}
