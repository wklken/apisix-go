package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialLogglyHTTPFixtureDeliveryNormalizesOnlyTimestampAndAddress(t *testing.T) {
	spec := differentialCasesForPlugin("loggly")[0]
	candidate, oracle := differentialLogglyComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialLogglyHTTPFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned Loggly observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("Loggly comparison mutated caller observations")
	}
}

func TestCompareDifferentialLogglyHTTPFixtureDeliveryRequiresObservedBehavior(t *testing.T) {
	spec := differentialCasesForPlugin("loggly")[0]
	candidate, oracle := differentialLogglyComparatorObservations(spec)
	candidate.UpstreamCalls = nil
	candidate.Upstream = DifferentialUpstreamObservation{}

	passed, _, err := compareDifferentialLogglyHTTPFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err == nil || passed || !strings.Contains(err.Error(), "exactly 2") {
		t.Fatalf("compare config-only observation = %t, %v, want captured behavior rejection", passed, err)
	}
}

func TestCompareDifferentialLogglyHTTPFixtureDeliveryRejectsLooseSemantics(t *testing.T) {
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
			name: "gateway 201 is not normalized to 200",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[0].Status = http.StatusCreated
			},
			want: "gateway step",
		},
		{
			name: "wrong origin host",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				differentialLoggerTestCall(candidate, "/logger/loggly").Host = candidate.UpstreamAddress
			},
			want: "origin request",
		},
		{
			name: "wrong bulk method",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialLoggerTestCall(oracle, differentialLogglyBulkPath).Method = http.MethodPut
			},
			want: "missing POST",
		},
		{
			name: "wrong bulk path",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialLoggerTestCall(candidate, differentialLogglyBulkPath)
				call.Path = "/loggly/bulk/other/tag/bulk"
				candidate.Upstream = *call
			},
			want: "missing POST",
		},
		{
			name: "wrong tag",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialLoggerTestCall(oracle, differentialLogglyBulkPath).Headers["X-LOGGLY-TAG"] = []string{"other"}
			},
			want: "X-LOGGLY-TAG",
		},
		{
			name: "extra semantic header",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				differentialLoggerTestCall(candidate, differentialLogglyBulkPath).Headers["Authorization"] = []string{"hidden"}
			},
			want: "unapproved semantic header",
		},
		{
			name: "wrong case",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialLoggerTestCall(oracle, differentialLogglyBulkPath)
				call.Body = strings.Replace(call.Body, `"case":"loggly"`, `"case":"other"`, 1)
			},
			want: `field "case"`,
		},
		{
			name: "wrong route",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialLoggerTestCall(candidate, differentialLogglyBulkPath)
				call.Body = strings.Replace(call.Body, `"route_id":"differential-loggly-http-delivery"`, `"route_id":"other"`, 1)
				candidate.Upstream = *call
			},
			want: "field route_id",
		},
		{
			name: "invalid timestamp",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialLoggerTestCall(candidate, differentialLogglyBulkPath)
				call.Body = strings.Replace(call.Body, "2026-08-28T10:11:12+08:00", "not-a-time", 1)
				candidate.Upstream = *call
			},
			want: "timestamp",
		},
		{
			name: "extra payload field",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialLoggerTestCall(oracle, differentialLogglyBulkPath)
				call.Body = strings.TrimSuffix(call.Body, "}") + `,"extra":true}`
			},
			want: "field",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("loggly")[0]
			candidate, oracle := differentialLogglyComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialLogglyHTTPFixtureDelivery(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose Loggly semantics = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialLogglyComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	loggerCall := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodPost,
		Path: differentialLogglyBulkPath, Host: "127.0.0.1:31007",
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"X-LOGGLY-TAG": {"apisix"},
		},
		Body: `{"case":"loggly","route_id":"` + spec.RouteID +
			`","timestamp":"2026-08-28T10:11:12+08:00"}`,
	}
	candidate, oracle := differentialLoggerComparatorPair(
		spec, "127.0.0.1:31007", "host.containers.internal:1980", loggerCall,
	)
	oracleCall := differentialLoggerTestCall(&oracle, differentialLogglyBulkPath)
	oracleCall.Body = strings.Replace(
		oracleCall.Body,
		"2026-08-28T10:11:12+08:00",
		"2026-08-28T02:11:13Z",
		1,
	)
	oracle.Upstream = oracle.UpstreamCalls[len(oracle.UpstreamCalls)-1]
	return candidate, oracle
}
