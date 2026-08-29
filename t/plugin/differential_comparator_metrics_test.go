package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialNodeStatusJSONCounters(t *testing.T) {
	spec, candidate, oracle := differentialNodeStatusComparatorTestObservations()
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialNodeStatusJSONCounters(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compare node-status counters: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("equivalent node-status counters rejected: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("node-status comparison mutated caller observations")
	}
}

func TestCompareDifferentialNodeStatusJSONCountersRejectsLooseInputs(t *testing.T) {
	assertRejected := func(
		t *testing.T,
		edit func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation),
	) {
		t.Helper()
		spec, candidate, oracle := differentialNodeStatusComparatorTestObservations()
		edit(&spec, &candidate, &oracle)
		passed, _, _ := compareDifferentialNodeStatusJSONCounters(
			spec,
			candidate,
			oracle,
			testNormalizationPolicy(),
		)
		if passed {
			t.Fatal("loose node-status input was normalized")
		}
	}

	for _, test := range []struct {
		name string
		edit func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
	}{
		{
			name: "unpinned case",
			edit: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.RouteID = "another-route"
			},
		},
		{
			name: "non-200 status on both sides",
			edit: func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
				candidate.Status = http.StatusCreated
				oracle.Status = http.StatusCreated
			},
		},
		{
			name: "upstream activity on both sides",
			edit: func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
				candidate.Upstream.Received = true
				oracle.Upstream.Received = true
			},
		},
		{
			name: "candidate NGINX-only counter",
			edit: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Body = `{"id":"11111111-1111-4111-8111-111111111111","status":{"active":"1","accepted":"2","handled":"2","total":"3","reading":"0"}}`
			},
		},
		{
			name: "invalid UUID",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Body = `{"id":"not-a-uuid","status":{"active":"4","accepted":"5","handled":"5","total":"6"}}`
			},
		},
		{
			name: "noncanonical decimal",
			edit: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Body = `{"id":"11111111-1111-4111-8111-111111111111","status":{"active":"01","accepted":"2","handled":"2","total":"3"}}`
			},
		},
		{
			name: "numeric counter",
			edit: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Body = `{"id":"11111111-1111-4111-8111-111111111111","status":{"active":1,"accepted":"2","handled":"2","total":"3"}}`
			},
		},
		{
			name: "unknown JSON field",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Body = `{"id":"22222222-2222-4222-8222-222222222222","status":{"active":"4","accepted":"5","handled":"5","total":"6","workers":"1"}}`
			},
		},
		{
			name: "duplicate JSON field",
			edit: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Body = `{"id":"11111111-1111-4111-8111-111111111111","status":{"active":"1","active":"2","accepted":"2","handled":"2","total":"3"}}`
			},
		},
		{
			name: "trailing JSON value",
			edit: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Body += ` {}`
			},
		},
		{
			name: "non-JSON content type",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Headers["Content-Type"] = []string{"application/json"}
			},
		},
		{
			name: "semantic header difference",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Headers["X-Semantic"] = []string{"changed"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertRejected(t, test.edit)
		})
	}
}

func TestCompareDifferentialPrometheusRouteStatusSeries(t *testing.T) {
	spec, candidate, oracle := differentialPrometheusComparatorTestObservations()
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialPrometheusRouteStatusSeries(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compare Prometheus route series: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("equivalent Prometheus route series rejected: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("Prometheus comparison mutated caller observations")
	}
}

func TestCompareDifferentialPrometheusRouteStatusSeriesRejectsLooseInputs(t *testing.T) {
	assertRejected := func(
		t *testing.T,
		edit func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation),
	) {
		t.Helper()
		spec, candidate, oracle := differentialPrometheusComparatorTestObservations()
		edit(&spec, &candidate, &oracle)
		passed, _, _ := compareDifferentialPrometheusRouteStatusSeries(
			spec,
			candidate,
			oracle,
			testNormalizationPolicy(),
		)
		if passed {
			t.Fatal("loose Prometheus input was normalized")
		}
	}

	for _, test := range []struct {
		name string
		edit func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
	}{
		{
			name: "unpinned second step",
			edit: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Steps[1].Request.Path = "/metrics"
			},
		},
		{
			name: "step zero wrong body on both sides",
			edit: func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
				candidate.Steps[0].Body = "changed"
				oracle.Steps[0].Body = "changed"
			},
		},
		{
			name: "missing upstream calls on both sides",
			edit: func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
				candidate.UpstreamCalls = nil
				oracle.UpstreamCalls = nil
				candidate.Upstream = DifferentialUpstreamObservation{}
				oracle.Upstream = DifferentialUpstreamObservation{}
			},
		},
		{
			name: "scrape is not text plain",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Steps[1].Headers["Content-Type"] = []string{"application/json"}
			},
		},
		{
			name: "malformed unrelated metric",
			edit: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[1].Body += "broken{label=\"unterminated} 1\n"
			},
		},
		{
			name: "missing target series",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Steps[1].Body = "other_process_metric 7\n"
			},
		},
		{
			name: "wrong target value",
			edit: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[1].Body = strings.Replace(
					candidate.Steps[1].Body,
					"} 1\n",
					"} 2\n",
					1,
				)
			},
		},
		{
			name: "duplicate target series",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Steps[1].Body += `apisix_http_status{code="200",route="differential-prometheus-route",node="second"} 1` + "\n"
			},
		},
		{
			name: "target missing code label",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Steps[1].Body = `apisix_http_status{route="differential-prometheus-route"} 1` + "\n"
			},
		},
		{
			name: "target duplicate code label",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Steps[1].Body = "# TYPE apisix_http_status counter\n" +
					`apisix_http_status{code="500",code="200",route="differential-prometheus-route"} 1` + "\n"
			},
		},
		{
			name: "semantic step header difference",
			edit: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Steps[1].Headers["X-Semantic"] = []string{"changed"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertRejected(t, test.edit)
		})
	}
}

func differentialNodeStatusComparatorTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	spec := differentialNodeStatusCases()[0]
	candidate := DifferentialObservation{
		Status: http.StatusOK,
		Headers: map[string][]string{
			"Content-Length": {"129"},
			"Content-Type":   {"text/plain"},
			"X-Semantic":     {"same"},
		},
		Body:             `{"id":"11111111-1111-4111-8111-111111111111","status":{"active":"1","accepted":"2","handled":"2","total":"3"}}`,
		Host:             spec.Request.Host,
		SecurityDecision: "not_applicable",
		Upstream:         DifferentialUpstreamObservation{},
	}
	oracle := copyDifferentialObservation(candidate)
	oracle.Headers["Content-Length"] = []string{"182"}
	oracle.Headers["Content-Type"] = []string{"text/plain; charset=utf-8"}
	oracle.Body = `{"id":"22222222-2222-4222-8222-222222222222","status":{"active":"4","accepted":"5","handled":"5","total":"6","reading":"1","writing":"2","waiting":"3"}}`
	return spec, candidate, oracle
}

func differentialPrometheusComparatorTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	spec := differentialPrometheusCases()[0]
	step := func(body, contentType, contentLength string) DifferentialStepObservation {
		return DifferentialStepObservation{
			Status: http.StatusOK,
			Headers: map[string][]string{
				"Content-Length": {contentLength},
				"Content-Type":   {contentType},
				"X-Semantic":     {"same"},
			},
			Body:             body,
			Host:             spec.Steps[0].Request.Host,
			SecurityDecision: "not_applicable",
		}
	}
	candidateMetrics := "# TYPE apisix_http_status counter\n" +
		`apisix_http_status{code="200",route="differential-prometheus-route",service="",node="candidate"} 1` + "\n" +
		"candidate_process_metric 9\n"
	oracleMetrics := "# HELP apisix_http_status HTTP status codes per service in APISIX\n" +
		"# TYPE apisix_http_status counter\n" +
		`apisix_http_status{node="oracle",route="differential-prometheus-route",code="200",consumer=""} 1` + "\n" +
		"oracle_process_metric 42\n"
	candidate := DifferentialObservation{
		Steps: []DifferentialStepObservation{
			step("profile-ok", "text/plain; charset=utf-8", "10"),
			step(candidateMetrics, "text/plain; version=0.0.4; charset=utf-8", "173"),
		},
		UpstreamFixture: "primary",
		UpstreamAddress: "127.0.0.1:31001",
		Upstream: DifferentialUpstreamObservation{
			Received: true,
			Fixture:  "primary",
			Method:   http.MethodGet,
			Path:     "/profile",
			Host:     "differential.example.test",
		},
	}
	candidate.UpstreamCalls = []DifferentialUpstreamObservation{candidate.Upstream}
	oracle := copyDifferentialObservation(candidate)
	oracle.Steps[1].Body = oracleMetrics
	oracle.Steps[1].Headers["Content-Length"] = []string{"221"}
	oracle.Steps[1].Headers["Content-Type"] = []string{"text/plain; charset=utf-8"}
	oracle.UpstreamAddress = "127.0.0.1:1980"
	return spec, candidate, oracle
}
