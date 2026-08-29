package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialErrorLogLoggerClickHouseDeliveryNormalizesOnlyRuntimeLogEnvelope(t *testing.T) {
	spec := differentialErrorLogLoggerCases()[0]
	candidate, oracle := differentialErrorLogLoggerComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialErrorLogLoggerClickHouseDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned error-log delivery = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("error-log comparison mutated caller observations")
	}
}

func TestCompareDifferentialErrorLogLoggerClickHouseDeliveryRequiresObservedDelivery(t *testing.T) {
	spec := differentialErrorLogLoggerCases()[0]
	candidate, oracle := differentialErrorLogLoggerComparatorObservations(spec)
	candidate.UpstreamCalls = candidate.UpstreamCalls[:1]
	candidate.Upstream = candidate.UpstreamCalls[0]

	passed, _, err := compareDifferentialErrorLogLoggerClickHouseDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err == nil || passed || !strings.Contains(err.Error(), "exactly 2") {
		t.Fatalf("compare origin-only observation = %t, %v, want real delivery rejection", passed, err)
	}
}

func TestCompareDifferentialErrorLogLoggerClickHouseDeliveryRejectsStartupSecurityWarningInsideRequestWindow(
	t *testing.T,
) {
	spec := differentialErrorLogLoggerCases()[0]
	candidate, oracle := differentialErrorLogLoggerComparatorObservations(spec)
	security := copyDifferentialUpstreamObservation(candidate.UpstreamCalls[1])
	security.Body = "INSERT INTO logs FORMAT JSONEachRow " +
		`{"data":"2026-08-28T02:11:11.123456789Z [warn] Using error-log-logger clickhouse.endpoint_addr with no TLS is a security risk"}`
	candidate.UpstreamCalls = append(candidate.UpstreamCalls, security)

	passed, _, err := compareDifferentialErrorLogLoggerClickHouseDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err == nil || passed || !strings.Contains(err.Error(), "exactly 2") {
		t.Fatalf("compare request-window startup warning = %t, %v, want strict rejection", passed, err)
	}
}

func TestCompareDifferentialErrorLogLoggerClickHouseDeliveryRejectsLooseSemantics(t *testing.T) {
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
			name: "gateway status remains exact",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[0].Status = http.StatusOK
			},
			want: "gateway step",
		},
		{
			name: "gateway semantic header is rejected",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[0].Headers["Content-Type"] = []string{"application/json"}
			},
			want: "gateway headers",
		},
		{
			name: "origin consumer identity remains exact",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				differentialErrorLogLoggerTestCall(candidate, http.MethodGet, "/warn").Headers["X-Consumer-Username"] = []string{"other"}
			},
			want: "origin headers",
		},
		{
			name: "logger method remains POST",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialErrorLogLoggerTestCall(oracle, http.MethodPost, differentialErrorLogLoggerClickHousePath).Method = http.MethodPut
			},
			want: "exactly one POST",
		},
		{
			name: "logger path remains exact",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialErrorLogLoggerTestCall(candidate, http.MethodPost, differentialErrorLogLoggerClickHousePath)
				call.Path = "/other"
				candidate.Upstream = *call
			},
			want: "exactly one POST",
		},
		{
			name: "ClickHouse key remains exact",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				differentialErrorLogLoggerTestCall(candidate, http.MethodPost, differentialErrorLogLoggerClickHousePath).
					Headers["X-ClickHouse-Key"] = []string{"wrong"}
			},
			want: "X-ClickHouse-Key",
		},
		{
			name: "extra semantic header is rejected",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialErrorLogLoggerTestCall(oracle, http.MethodPost, differentialErrorLogLoggerClickHousePath).
					Headers["Authorization"] = []string{"hidden"}
			},
			want: "unapproved semantic header",
		},
		{
			name: "table remains exact",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialErrorLogLoggerTestLogCall(candidate, differentialErrorLogLoggerWarning)
				call.Body = strings.Replace(call.Body, "INSERT INTO logs", "INSERT INTO other", 1)
				candidate.Upstream = *call
			},
			want: "prefix",
		},
		{
			name: "candidate severity remains warn",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialErrorLogLoggerTestLogCall(candidate, differentialErrorLogLoggerWarning)
				call.Body = strings.Replace(call.Body, "[warn]", "[error]", 1)
				candidate.Upstream = *call
			},
			want: "candidate log line",
		},
		{
			name: "candidate timestamp must be RFC3339Nano",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialErrorLogLoggerTestLogCall(candidate, differentialErrorLogLoggerWarning)
				call.Body = strings.Replace(call.Body, "2026-08-28T02:11:12.123456789Z", "not-a-time", 1)
				candidate.Upstream = *call
			},
			want: "candidate log line",
		},
		{
			name: "oracle warning message remains exact",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialErrorLogLoggerTestLogCall(oracle, differentialErrorLogLoggerWarning)
				call.Body = strings.Replace(call.Body, "Invalid authorization header format", "different warning", 1)
			},
			want: "oracle log line",
		},
		{
			name: "oracle request id remains a lowercase hex identifier",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialErrorLogLoggerTestLogCall(oracle, differentialErrorLogLoggerWarning)
				call.Body = strings.Replace(call.Body, strings.Repeat("a", 32), "not-a-request-id", 1)
			},
			want: "oracle log line",
		},
		{
			name: "multiple JSONEachRow entries are rejected",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialErrorLogLoggerTestLogCall(candidate, differentialErrorLogLoggerWarning)
				call.Body += `\n{"data":"2026-08-28T02:11:13Z [warn] unrelated"}`
				candidate.Upstream = *call
			},
			want: "JSON",
		},
		{
			name: "extra JSONEachRow field is rejected",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialErrorLogLoggerTestLogCall(oracle, differentialErrorLogLoggerWarning)
				call.Body = strings.TrimSuffix(call.Body, "}") + `,"extra":true}`
			},
			want: "field",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialErrorLogLoggerCases()[0]
			candidate, oracle := differentialErrorLogLoggerComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialErrorLogLoggerClickHouseDelivery(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose error-log semantics = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialErrorLogLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	origin := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodGet,
		Path: "/warn", Host: "differential.example.test",
		Headers: map[string][]string{"X-Consumer-Username": {"anonymous"}},
	}
	candidateLogger := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodPost,
		Path: differentialErrorLogLoggerClickHousePath, Host: "127.0.0.1:31008",
		Headers: differentialErrorLogLoggerClickHouseHeaders(),
		Body: "INSERT INTO logs FORMAT JSONEachRow " +
			`{"data":"2026-08-28T02:11:12.123456789Z [warn] Invalid authorization header format"}`,
	}
	candidate := DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status: http.StatusNoContent,
			Headers: map[string][]string{
				"Date":   {"Fri, 28 Aug 2026 02:11:12 GMT"},
				"Server": {"APISIX/test-build"},
			},
			Host: "gateway.example.test", SecurityDecision: "allow",
		}},
		UpstreamFixture: spec.Fixture.Name, UpstreamAddress: "127.0.0.1:31008",
		UpstreamCalls: []DifferentialUpstreamObservation{origin, candidateLogger},
		Upstream:      candidateLogger,
	}

	oracleLogger := copyDifferentialUpstreamObservation(candidateLogger)
	oracleLogger.Host = "host.containers.internal:1980"
	oracleLogger.Body = "INSERT INTO logs FORMAT JSONEachRow " +
		`{"data":"2026/08/28 02:11:13 [warn] 123#123: *7 [lua] basic-auth.lua:136: find_consumer(): Invalid authorization header format, client: 192.0.2.10, server: _, request: \"GET /warn HTTP/1.1\", host: \"gateway.example.test\", request_id: \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\""}`
	oracle := copyDifferentialObservation(candidate)
	oracle.Steps[0].Headers = map[string][]string{
		"Content-Length": {"0"},
		"Server":         {"APISIX/3.17.0"},
	}
	oracle.UpstreamAddress = "host.containers.internal:1980"
	oracle.UpstreamCalls = []DifferentialUpstreamObservation{origin, oracleLogger}
	oracle.Upstream = origin
	return candidate, oracle
}

func differentialErrorLogLoggerClickHouseHeaders() map[string][]string {
	return map[string][]string{
		"Content-Type":          {"application/json"},
		"X-ClickHouse-User":     {"default"},
		"X-ClickHouse-Key":      {"differential-password"},
		"X-ClickHouse-Database": {"default"},
	}
}

func copyDifferentialUpstreamObservation(
	observation DifferentialUpstreamObservation,
) DifferentialUpstreamObservation {
	observation.Headers = copyDifferentialHeaders(observation.Headers)
	return observation
}

func differentialErrorLogLoggerTestCall(
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
	panic("missing differential error-log-logger test call " + method + " " + path)
}

func differentialErrorLogLoggerTestLogCall(
	observation *DifferentialObservation,
	message string,
) *DifferentialUpstreamObservation {
	for index := range observation.UpstreamCalls {
		call := &observation.UpstreamCalls[index]
		if call.Method == http.MethodPost && call.Path == differentialErrorLogLoggerClickHousePath &&
			strings.Contains(call.Body, message) {
			return call
		}
	}
	panic("missing differential error-log-logger test log " + message)
}
