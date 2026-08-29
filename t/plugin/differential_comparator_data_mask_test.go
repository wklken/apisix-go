package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialDataMaskRequestLineAcceptsPinnedSemanticCalls(t *testing.T) {
	spec := differentialDataMaskCases()[0]
	candidate, oracle := differentialDataMaskComparatorObservations()
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialDataMaskRequestLine(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned data-mask observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("data-mask comparison mutated caller observations")
	}
}

func TestCompareDifferentialDataMaskRequestLineRejectsLooseCallContracts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialObservation)
		want   string
	}{
		{
			name: "missing logger call",
			mutate: func(observation *DifferentialObservation) {
				observation.UpstreamCalls = observation.UpstreamCalls[:1]
			},
			want: "exactly 2",
		},
		{
			name: "changed origin query",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodGet).Path = "/hello?token=%2A%2A%2A%2A%2A"
			},
			want: "missing GET /hello?password=secret&token=mytoken",
		},
		{
			name: "origin semantic header",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodGet).Headers = map[string][]string{"Content-Type": {"application/json"}}
			},
			want: "origin request",
		},
		{
			name: "origin body",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodGet).Body = "unexpected"
			},
			want: "origin request",
		},
		{
			name: "logger path",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodPost).Path = "/other"
			},
			want: "missing POST /logs",
		},
		{
			name: "logger content type",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodPost).Headers["Content-Type"] = []string{"text/plain"}
			},
			want: "Content-Type",
		},
		{
			name: "logger extra semantic header",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodPost).Headers["Authorization"] = []string{"secret"}
			},
			want: "unapproved semantic header",
		},
		{
			name: "logger leaks password",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodPost).Body = `{"request_line":"GET /hello?password=secret&token=***** HTTP/1.1","route_id":"differential-data-mask-request-line"}`
			},
			want: "request_line",
		},
		{
			name: "logger leaves token clear",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodPost).Body = `{"request_line":"GET /hello?token=mytoken HTTP/1.1","route_id":"differential-data-mask-request-line"}`
			},
			want: "request_line",
		},
		{
			name: "logger missing route ID",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodPost).Body = `{"request_line":"GET /hello?token=***** HTTP/1.1"}`
			},
			want: "route_id",
		},
		{
			name: "logger wrong route ID",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodPost).Body = `{"request_line":"GET /hello?token=***** HTTP/1.1","route_id":"other-route"}`
			},
			want: "route_id",
		},
		{
			name: "logger extra JSON field",
			mutate: func(observation *DifferentialObservation) {
				differentialDataMaskTestCall(observation, http.MethodPost).Body = `{"request_line":"GET /hello?token=***** HTTP/1.1","route_id":"differential-data-mask-request-line","extra":true}`
			},
			want: "unknown field",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialDataMaskCases()[0]
			candidate, oracle := differentialDataMaskComparatorObservations()
			test.mutate(&candidate)
			candidate.Upstream = candidate.UpstreamCalls[len(candidate.UpstreamCalls)-1]

			passed, _, err := compareDifferentialDataMaskRequestLine(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose %s = %t, %v, want error containing %q", test.name, passed, err, test.want)
			}
		})
	}
}

func TestCompareDifferentialDataMaskRequestLineRejectsUnpinnedCase(t *testing.T) {
	spec := differentialDataMaskCases()[0]
	spec.Fixture.ExpectedCalls++
	candidate, oracle := differentialDataMaskComparatorObservations()

	passed, _, err := compareDifferentialDataMaskRequestLine(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err == nil || passed || !strings.Contains(err.Error(), "exact pinned data-mask") {
		t.Fatalf("compare unpinned data-mask case = %t, %v", passed, err)
	}
}

func differentialDataMaskComparatorObservations() (DifferentialObservation, DifferentialObservation) {
	origin := DifferentialUpstreamObservation{
		Received: true, Fixture: "origin-and-data-mask-log", Method: http.MethodGet,
		Path: "/hello?password=secret&token=mytoken", Host: "differential.example.test",
	}
	logger := DifferentialUpstreamObservation{
		Received: true, Fixture: "origin-and-data-mask-log", Method: http.MethodPost,
		Path: "/logs", Host: "127.0.0.1:31150",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    `{"request_line":"GET /hello?token=***** HTTP/1.1","route_id":"differential-data-mask-request-line"}`,
	}
	candidate := DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status: http.StatusOK, Body: "done", Host: "gateway.example.test",
			SecurityDecision: "not_applicable",
		}},
		UpstreamFixture: "origin-and-data-mask-log",
		UpstreamAddress: "127.0.0.1:31150",
		UpstreamCalls:   []DifferentialUpstreamObservation{origin, logger},
		Upstream:        logger,
	}
	oracle := copyDifferentialObservation(candidate)
	oracle.UpstreamAddress = "host.containers.internal:1980"
	oracle.UpstreamCalls[1].Host = "host.containers.internal"
	oracle.UpstreamCalls[0], oracle.UpstreamCalls[1] = oracle.UpstreamCalls[1], oracle.UpstreamCalls[0]
	oracle.Upstream = oracle.UpstreamCalls[1]
	return candidate, oracle
}

func differentialDataMaskTestCall(
	observation *DifferentialObservation,
	method string,
) *DifferentialUpstreamObservation {
	for index := range observation.UpstreamCalls {
		if observation.UpstreamCalls[index].Method == method {
			return &observation.UpstreamCalls[index]
		}
	}
	panic("missing test fixture call for method " + method)
}
