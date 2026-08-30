package pluginintegration

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialGoogleCloudLoggingNormalizesOnlySignedJWTDynamicsAndEntryEnvelope(t *testing.T) {
	spec := differentialCasesForPlugin("google-cloud-logging")[0]
	candidate, oracle := differentialGoogleCloudLoggingComparatorObservations(spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialGoogleCloudLoggingFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned Google logging observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("Google logging comparison mutated caller observations")
	}
}

func TestCompareDifferentialGoogleCloudLoggingRejectsLooseOAuthAndEntryContracts(t *testing.T) {
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
			name: "missing token call",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls = append(candidate.UpstreamCalls[:1], candidate.UpstreamCalls[2:]...)
				candidate.Upstream = candidate.UpstreamCalls[len(candidate.UpstreamCalls)-1]
			},
			want: "exactly 3",
		},
		{
			name: "gateway semantic header",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Steps[0].Headers["X-Unapproved"] = []string{"value"}
			},
			want: "gateway headers",
		},
		{
			name: "wrong grant type",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialGoogleCloudLoggingTestCall(candidate, http.MethodPost, differentialGoogleCloudLoggingTokenPath)
				form, _ := url.ParseQuery(call.Body)
				form.Set("grant_type", "client_credentials")
				call.Body = form.Encode()
			},
			want: "grant_type",
		},
		{
			name: "extra form field",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialGoogleCloudLoggingTestCall(oracle, http.MethodPost, differentialGoogleCloudLoggingTokenPath)
				call.Body += "&extra=value"
				oracle.Upstream = *call
			},
			want: "form fields",
		},
		{
			name: "JWT issuer",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				setDifferentialGoogleCloudLoggingJWTClaims(candidate, `{"iss":"other@example.org","aud":"http://127.0.0.1:31160/token","scope":"https://apisix.apache.org/logs:admin","iat":1700000000,"exp":1700003600}`)
			},
			want: "iss",
		},
		{
			name: "JWT audience",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				setDifferentialGoogleCloudLoggingJWTClaims(oracle, `{"iss":"differential@example.iam.gserviceaccount.com","aud":"https://oauth2.googleapis.com/token","scope":"https://apisix.apache.org/logs:admin","iat":1700000100,"exp":1700003700}`)
			},
			want: "aud",
		},
		{
			name: "JWT scope",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				setDifferentialGoogleCloudLoggingJWTClaims(candidate, `{"iss":"differential@example.iam.gserviceaccount.com","aud":"http://127.0.0.1:31160/token","scope":"other","iat":1700000000,"exp":1700003600}`)
			},
			want: "scope",
		},
		{
			name: "JWT lifetime",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				setDifferentialGoogleCloudLoggingJWTClaims(candidate, `{"iss":"differential@example.iam.gserviceaccount.com","aud":"http://127.0.0.1:31160/token","scope":"https://apisix.apache.org/logs:admin","iat":1700000000,"exp":1700007200}`)
			},
			want: "exp-iat",
		},
		{
			name: "extra JWT claim",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				setDifferentialGoogleCloudLoggingJWTClaims(candidate, `{"iss":"differential@example.iam.gserviceaccount.com","sub":"differential@example.iam.gserviceaccount.com","aud":"http://127.0.0.1:31160/token","scope":"https://apisix.apache.org/logs:admin","iat":1700000000,"exp":1700003600}`)
			},
			want: "unknown field",
		},
		{
			name: "entries authorization",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialGoogleCloudLoggingTestCall(oracle, http.MethodPost, differentialGoogleCloudLoggingEntriesPath).
					Headers["Authorization"] = []string{"Bearer wrong"}
			},
			want: "Authorization",
		},
		{
			name: "entries logName",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialGoogleCloudLoggingTestCall(candidate, http.MethodPost, differentialGoogleCloudLoggingEntriesPath)
				call.Body = strings.Replace(call.Body, "projects/differential-project/logs/differential-log", "projects/other/logs/differential-log", 1)
				candidate.Upstream = *call
			},
			want: "logName",
		},
		{
			name: "entries resource",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialGoogleCloudLoggingTestCall(oracle, http.MethodPost, differentialGoogleCloudLoggingEntriesPath)
				call.Body = strings.Replace(call.Body, `"type":"global"`, `"type":"gce_instance"`, 1)
			},
			want: "resource",
		},
		{
			name: "entries custom payload",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialGoogleCloudLoggingTestCall(candidate, http.MethodPost, differentialGoogleCloudLoggingEntriesPath)
				call.Body = strings.Replace(call.Body, `"case":"google-cloud-logging"`, `"case":"wrong"`, 1)
				candidate.Upstream = *call
			},
			want: "jsonPayload",
		},
		{
			name: "second entry",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialGoogleCloudLoggingTestCall(candidate, http.MethodPost, differentialGoogleCloudLoggingEntriesPath)
				call.Body = strings.Replace(call.Body, `],"partialSuccess"`, `,{"jsonPayload":{}}],"partialSuccess"`, 1)
				candidate.Upstream = *call
			},
			want: "exactly one",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("google-cloud-logging")[0]
			candidate, oracle := differentialGoogleCloudLoggingComparatorObservations(spec)
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialGoogleCloudLoggingFixtureDelivery(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose Google logging contract = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialGoogleCloudLoggingComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	candidateAddress := "127.0.0.1:31160"
	candidateCalls := []DifferentialUpstreamObservation{
		differentialGoogleCloudLoggingOriginCall(spec),
		differentialGoogleCloudLoggingTokenCall(candidateAddress, 1700000000, strings.Repeat("a", 128)),
		differentialGoogleCloudLoggingEntriesCall(
			spec,
			candidateAddress,
			"2026-08-29T01:02:03.123456789Z",
			"candidate-request-id",
		),
	}
	candidate := differentialGoogleCloudLoggingObservation(spec, candidateAddress, candidateCalls)
	candidate.Steps[0].Headers = map[string][]string{
		"Content-Length": {"84"},
		"Content-Type":   {"application/json"},
		"Date":           {"Sat, 29 Aug 2026 01:02:03 GMT"},
		"Server":         {"APISIX/test-build"},
	}

	oracleAddress := "host.containers.internal:1980"
	oracleCalls := []DifferentialUpstreamObservation{
		differentialGoogleCloudLoggingTokenCall(oracleAddress, 1700000100, strings.Repeat("b", 128)),
		differentialGoogleCloudLoggingOriginCall(spec),
		differentialGoogleCloudLoggingEntriesCall(spec, oracleAddress, "2026-08-29T01:02:04Z", "oracle-request-id"),
	}
	oracle := differentialGoogleCloudLoggingObservation(spec, oracleAddress, oracleCalls)
	oracle.Steps[0].Headers = map[string][]string{
		"Content-Length": {"84"},
		"Content-Type":   {"application/json"},
		"Server":         {"APISIX/3.17.0"},
	}
	oracle.Upstream = oracleCalls[0]
	return candidate, oracle
}

func differentialGoogleCloudLoggingObservation(
	spec DifferentialCase,
	address string,
	calls []DifferentialUpstreamObservation,
) DifferentialObservation {
	return DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status:  http.StatusOK,
			Headers: map[string][]string{"Content-Type": {"application/json"}},
			Body:    spec.Fixture.Response.Body, Host: spec.Steps[0].Request.Host,
			SecurityDecision: spec.Steps[0].SecurityDecision,
		}},
		UpstreamFixture: spec.Fixture.Name,
		UpstreamAddress: address,
		UpstreamCalls:   calls,
		Upstream:        calls[len(calls)-1],
	}
}

func differentialGoogleCloudLoggingOriginCall(spec DifferentialCase) DifferentialUpstreamObservation {
	return DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodGet,
		Path: spec.Steps[0].Request.Path, Host: "differential.example.test",
	}
}

func differentialGoogleCloudLoggingTokenCall(
	address string,
	iat int64,
	signature string,
) DifferentialUpstreamObservation {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(
		`{"iss":"` + differentialGoogleCloudLoggingClientEmail +
			`","aud":"http://` + address + differentialGoogleCloudLoggingTokenPath +
			`","scope":"` + differentialGoogleCloudLoggingScope + `","iat":` +
			itoaDifferentialGoogleCloudLogging(iat) + `,"exp":` +
			itoaDifferentialGoogleCloudLogging(iat+3600) + `}`,
	))
	encodedSignature := base64.RawURLEncoding.EncodeToString([]byte(signature))
	form := url.Values{
		"assertion":  {header + "." + claims + "." + encodedSignature},
		"grant_type": {differentialGoogleCloudLoggingJWTBearerGrantType},
	}
	return DifferentialUpstreamObservation{
		Received: true, Fixture: "origin-and-google-cloud-logging", Method: http.MethodPost,
		Path: differentialGoogleCloudLoggingTokenPath, Host: address,
		Headers: map[string][]string{"Content-Type": {"application/x-www-form-urlencoded"}},
		Body:    form.Encode(),
	}
}

func differentialGoogleCloudLoggingEntriesCall(
	spec DifferentialCase,
	address string,
	timestamp string,
	insertID string,
) DifferentialUpstreamObservation {
	body := `{"entries":[{"jsonPayload":{"case":"google-cloud-logging","route_id":"` + spec.RouteID +
		`"},"labels":{"source":"apache-apisix-google-cloud-logging"},"timestamp":"` + timestamp +
		`","resource":{"type":"global","labels":{"project_id":"` + differentialGoogleCloudLoggingProjectID +
		`"}},"insertId":"` + insertID + `","logName":"projects/` + differentialGoogleCloudLoggingProjectID +
		`/logs/` + differentialGoogleCloudLoggingLogID + `"}],"partialSuccess":false}`
	return DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodPost,
		Path: differentialGoogleCloudLoggingEntriesPath, Host: address,
		Headers: map[string][]string{
			"Authorization": {"Bearer differential-access-token"},
			"Content-Type":  {"application/json"},
		},
		Body: body,
	}
}

func setDifferentialGoogleCloudLoggingJWTClaims(observation *DifferentialObservation, claims string) {
	call := differentialGoogleCloudLoggingTestCall(
		observation,
		http.MethodPost,
		differentialGoogleCloudLoggingTokenPath,
	)
	form, _ := url.ParseQuery(call.Body)
	parts := strings.Split(form.Get("assertion"), ".")
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(claims))
	form.Set("assertion", strings.Join(parts, "."))
	call.Body = form.Encode()
	observation.Upstream = *call
}

func differentialGoogleCloudLoggingTestCall(
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
	panic("missing Google Cloud Logging test call " + method + " " + path)
}

func itoaDifferentialGoogleCloudLogging(value int64) string {
	return fmt.Sprintf("%d", value)
}
