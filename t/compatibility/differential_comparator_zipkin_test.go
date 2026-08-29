package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialZipkinV2ServerSpanCoreAcceptsOnlyDocumentedDynamicsAndV2Gap(t *testing.T) {
	spec := differentialCasesForPlugin("zipkin")[0]
	candidate, oracle := differentialZipkinComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialZipkinV2ServerSpanCore(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned Zipkin observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("Zipkin comparison mutated caller observations")
	}
}

func TestCompareDifferentialZipkinV2ServerSpanCoreRejectsLooseContracts(t *testing.T) {
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
			name: "gateway status",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[0].Status = http.StatusCreated
			},
			want: "gateway step",
		},
		{
			name: "missing collector behavior",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls = candidate.UpstreamCalls[:1]
				candidate.Upstream = candidate.UpstreamCalls[0]
			},
			want: "exactly 2",
		},
		{
			name: "wrong propagated trace",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				zipkinTestCall(oracle, http.MethodGet, "/zipkin/v2").Headers["X-B3-TraceId"] = []string{"463ac35c9f6413ad"}
			},
			want: "trace",
		},
		{
			name: "sampling disabled",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodGet, "/zipkin/v2")
				call.Headers["X-B3-Sampled"] = []string{"0"}
			},
			want: "sampled",
		},
		{
			name: "debug flag missing",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodGet, "/zipkin/v2")
				delete(call.Headers, "X-B3-Flags")
			},
			want: "header count",
		},
		{
			name: "wrong origin parent relationship",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodGet, "/zipkin/v2")
				call.Headers["X-B3-ParentSpanId"] = []string{"05e3ac9a4f6e3b90"}
			},
			want: "parent",
		},
		{
			name: "wrong collector method",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				zipkinTestCall(oracle, http.MethodPost, differentialZipkinCollectorPath).Method = http.MethodPut
			},
			want: "missing POST",
		},
		{
			name: "wrong collector path",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodPost, differentialZipkinCollectorPath)
				call.Path = "/api/v1/spans"
				candidate.Upstream = *call
			},
			want: "missing POST",
		},
		{
			name: "wrong content type",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				zipkinTestCall(oracle, http.MethodPost, differentialZipkinCollectorPath).Headers["Content-Type"] = []string{"application/x-json"}
			},
			want: "Content-Type",
		},
		{
			name: "wrong server status tag",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodPost, differentialZipkinCollectorPath)
				call.Body = strings.Replace(call.Body, `"http.status_code":"200"`, `"http.status_code":"201"`, 1)
				candidate.Upstream = *call
			},
			want: "http.status_code",
		},
		{
			name: "missing response source tag",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodPost, differentialZipkinCollectorPath)
				call.Body = strings.Replace(call.Body, `,"apisix.response_source":"upstream"`, "", 1)
				candidate.Upstream = *call
			},
			want: "missing tag",
		},
		{
			name: "undeclared extension tag",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodPost, differentialZipkinCollectorPath)
				call.Body = strings.Replace(call.Body, `"component":"apisix"`, `"component":"apisix","apisix.node_id":"node"`, 1)
				candidate.Upstream = *call
			},
			want: "unknown tag",
		},
		{
			name: "wrong service",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := zipkinTestCall(oracle, http.MethodPost, differentialZipkinCollectorPath)
				call.Body = strings.Replace(call.Body, `"serviceName":"APISIX"`, `"serviceName":"other"`, 1)
			},
			want: "serviceName",
		},
		{
			name: "wrong server parent",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodPost, differentialZipkinCollectorPath)
				call.Body = strings.Replace(call.Body, `"parentId":"e457b5a2e4d86bd1"`, `"parentId":"05e3ac9a4f6e3b90"`, 1)
				candidate.Upstream = *call
			},
			want: "parentId",
		},
		{
			name: "unknown server field",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := zipkinTestCall(candidate, http.MethodPost, differentialZipkinCollectorPath)
				call.Body = strings.Replace(call.Body, `"kind":"SERVER"`, `"kind":"SERVER","shared":true`, 1)
				candidate.Upstream = *call
			},
			want: "unknown field",
		},
		{
			name: "v1 phase topology is not accepted as v2",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := zipkinTestCall(oracle, http.MethodPost, differentialZipkinCollectorPath)
				call.Body = strings.TrimSuffix(call.Body, "]") +
					`,{"traceId":"80f198ee56343ba864fe8b2a57d3eff7","name":"apisix.rewrite","parentId":"bbbbbbbbbbbbbbbb","id":"eeeeeeeeeeeeeeee","timestamp":1700000000000000,"duration":1,"localEndpoint":{"serviceName":"APISIX","ipv4":"127.0.0.1","port":1984},"remoteEndpoint":null,"tags":{}},{"traceId":"80f198ee56343ba864fe8b2a57d3eff7","name":"apisix.access","parentId":"bbbbbbbbbbbbbbbb","id":"ffffffffffffffff","timestamp":1700000000000000,"duration":1,"localEndpoint":{"serviceName":"APISIX","ipv4":"127.0.0.1","port":1984},"remoteEndpoint":null,"tags":{}}]`
			},
			want: "three v2 spans",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("zipkin")[0]
			candidate, oracle := differentialZipkinComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialZipkinV2ServerSpanCore(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose Zipkin contract = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialZipkinComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	const (
		candidateServerID = "aaaaaaaaaaaaaaaa"
		oracleServerID    = "bbbbbbbbbbbbbbbb"
		oracleProxyID     = "cccccccccccccccc"
	)
	candidateOrigin := differentialZipkinOriginCall(spec, candidateServerID, differentialZipkinIncomingSpanID)
	candidateCollector := differentialZipkinCollectorCall(
		spec,
		"127.0.0.1:31121",
		`[{"traceId":"`+differentialZipkinTraceID+`","name":"apisix.request","parentId":"`+
			differentialZipkinIncomingSpanID+`","id":"`+candidateServerID+
			`","kind":"SERVER","timestamp":1700000000000000,"duration":1234,`+
			`"localEndpoint":{"serviceName":"APISIX","ipv4":"127.0.0.1","port":9080},`+
			`"remoteEndpoint":{"ipv4":"127.0.0.1","port":43111},`+
			`"tags":{"component":"apisix","http.method":"GET","http.url":"/zipkin/v2",`+
			`"http.status_code":"200","apisix.response_source":"upstream"}}]`,
	)
	candidate := differentialZipkinObservation(
		spec, "127.0.0.1:31121", candidateOrigin, candidateCollector,
	)

	oracleOrigin := differentialZipkinOriginCall(spec, oracleProxyID, oracleServerID)
	oracleCollector := differentialZipkinCollectorCall(
		spec,
		"127.0.0.1:1980",
		`[{"traceId":"`+differentialZipkinTraceID+`","name":"apisix.proxy","parentId":"`+
			oracleServerID+`","id":"`+oracleProxyID+
			`","timestamp":1700000000001000,"duration":1000,"localEndpoint":{"serviceName":"APISIX",`+
			`"ipv4":"127.0.0.1","port":1984},"remoteEndpoint":null,"tags":{}},`+
			`{"traceId":"`+differentialZipkinTraceID+`","name":"apisix.response_span","parentId":"`+
			oracleServerID+`","id":"dddddddddddddddd","timestamp":1700000000002000,"duration":500,`+
			`"localEndpoint":{"serviceName":"APISIX","ipv4":"127.0.0.1","port":1984},`+
			`"remoteEndpoint":null,"tags":{}},`+
			`{"traceId":"`+differentialZipkinTraceID+`","name":"apisix.request","parentId":"`+
			differentialZipkinIncomingSpanID+`","id":"`+oracleServerID+
			`","kind":"SERVER","timestamp":1700000000000001,"duration":4000,`+
			`"localEndpoint":{"serviceName":"APISIX","ipv4":"127.0.0.1","port":1984},`+
			`"remoteEndpoint":{"ipv4":"10.88.0.2","port":51234},`+
			`"tags":{"component":"apisix","http.method":"GET","http.url":"/zipkin/v2",`+
			`"http.status_code":"200","apisix.response_source":"upstream"}}]`,
	)
	oracle := differentialZipkinObservation(
		spec, "127.0.0.1:1980", oracleCollector, oracleOrigin,
	)
	return candidate, oracle
}

func differentialZipkinObservation(
	spec DifferentialCase,
	address string,
	calls ...DifferentialUpstreamObservation,
) DifferentialObservation {
	return DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status: spec.Fixture.Response.Status, Body: spec.Fixture.Response.Body,
			Host: spec.Steps[0].Request.Host, SecurityDecision: spec.Steps[0].SecurityDecision,
		}},
		UpstreamFixture: spec.Fixture.Name,
		UpstreamAddress: address,
		UpstreamCalls:   calls,
		Upstream:        calls[len(calls)-1],
	}
}

func differentialZipkinOriginCall(
	spec DifferentialCase,
	spanID string,
	parentID string,
) DifferentialUpstreamObservation {
	return DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodGet,
		Path: spec.Steps[0].Request.Path, Host: "differential.example.test",
		Headers: map[string][]string{
			"X-B3-TraceId":      {differentialZipkinTraceID},
			"X-B3-SpanId":       {spanID},
			"X-B3-ParentSpanId": {parentID},
			"X-B3-Sampled":      {"1"},
			"X-B3-Flags":        {"1"},
		},
	}
}

func differentialZipkinCollectorCall(
	spec DifferentialCase,
	host string,
	body string,
) DifferentialUpstreamObservation {
	return DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodPost,
		Path: differentialZipkinCollectorPath, Host: host,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    body,
	}
}

func zipkinTestCall(
	observation *DifferentialObservation,
	method string,
	path string,
) *DifferentialUpstreamObservation {
	for index := range observation.UpstreamCalls {
		call := &observation.UpstreamCalls[index]
		if call.Method == method && call.Path == path {
			return call
		}
	}
	panic("missing Zipkin test call " + method + " " + path)
}
