package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialAIAWSComprehendSigV4(t *testing.T) {
	spec, candidate, oracle := differentialAIAWSComprehendSigV4TestObservations()
	candidateBefore := cloneDifferentialObservation(candidate)
	oracleBefore := cloneDifferentialObservation(oracle)

	equal, detail, err := compareDifferentialAIAWSComprehendSigV4(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil || !equal || detail != "" {
		t.Fatalf("compare pinned Comprehend observations = %t, %q, %v", equal, detail, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("Comprehend comparison mutated caller observations")
	}
}

func TestCompareDifferentialAIAWSComprehendSigV4RejectsMalformedContract(t *testing.T) {
	assertError := func(
		t *testing.T,
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation),
		want string,
	) {
		t.Helper()
		spec, candidate, oracle := differentialAIAWSComprehendSigV4TestObservations()
		mutate(&spec, &candidate, &oracle)
		equal, _, err := compareDifferentialAIAWSComprehendSigV4(
			spec,
			candidate,
			oracle,
			testNormalizationPolicy(),
		)
		if err == nil || equal || !strings.Contains(err.Error(), want) {
			t.Fatalf("compare malformed contract = %t, %v, want error containing %q", equal, err, want)
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned spec",
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Request.Body = "clean"
			},
			want: "pinned",
		},
		{
			name: "wrong response status",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Status = http.StatusForbidden
			},
			want: "400",
		},
		{
			name: "wrong blocking body",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Body = "request body exceeds PROFANITY threshold"
			},
			want: "blocking response",
		},
		{
			name: "wrong security decision",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.SecurityDecision = "allow"
			},
			want: "security decision",
		},
		{
			name: "missing fixture request",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Upstream.Received = false
			},
			want: "one identified candidate fixture request",
		},
		{
			name: "fixture identity mismatch",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamFixture = "secondary"
			},
			want: "one identified oracle fixture request",
		},
		{
			name: "recorded retry",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.RetryCount = 1
			},
			want: "exactly one candidate fixture request",
		},
		{
			name: "sequence call mixed into single request",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls = []DifferentialUpstreamObservation{oracle.Upstream}
			},
			want: "exactly one oracle fixture request",
		},
		{
			name: "wrong dependency method",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Upstream.Method = http.MethodGet
			},
			want: "POST /",
		},
		{
			name: "wrong dependency path",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Upstream.Path = "/detect"
			},
			want: "POST /",
		},
		{
			name: "fixture Host does not equal address",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Upstream.Host = "comprehend.amazonaws.com"
			},
			want: "upstream Host",
		},
		{
			name: "wrong Comprehend language",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Upstream.Body = `{"LanguageCode":"fr","TextSegments":[{"Text":"toxic"}]}`
			},
			want: "Comprehend request body",
		},
		{
			name: "extra Comprehend field",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Upstream.Body = `{"LanguageCode":"en","TextSegments":[{"Text":"toxic"}],"Extra":true}`
			},
			want: "Comprehend request body",
		},
		{
			name: "missing Authorization",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Upstream.Headers = nil
			},
			want: "Authorization",
		},
		{
			name: "wrong credential access key",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				setDifferentialAWSAuthorization(oracle, strings.Replace(
					oracle.Upstream.Headers["Authorization"][0],
					"Credential=access/",
					"Credential=other/",
					1,
				))
			},
			want: "Authorization",
		},
		{
			name: "nondigit credential date",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				setDifferentialAWSAuthorization(candidate, strings.Replace(
					candidate.Upstream.Headers["Authorization"][0],
					"20260828",
					"2026-828",
					1,
				))
			},
			want: "Authorization",
		},
		{
			name: "wrong credential region",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				setDifferentialAWSAuthorization(oracle, strings.Replace(
					oracle.Upstream.Headers["Authorization"][0],
					"/us-east-1/",
					"/us-west-2/",
					1,
				))
			},
			want: "Authorization",
		},
		{
			name: "extra signed header",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				setDifferentialAWSAuthorization(candidate, strings.Replace(
					candidate.Upstream.Headers["Authorization"][0],
					"x-amz-date;x-amz-target",
					"x-amz-date;x-amz-security-token;x-amz-target",
					1,
				))
			},
			want: "Authorization",
		},
		{
			name: "uppercase signature",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				setDifferentialAWSAuthorization(oracle, strings.ReplaceAll(
					oracle.Upstream.Headers["Authorization"][0],
					"b",
					"B",
				))
			},
			want: "Authorization",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertError(t, test.mutate, test.want)
		})
	}
}

func TestCompareDifferentialAIAWSComprehendSigV4KeepsNonvolatileFieldsStrict(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialObservation)
	}{
		{
			name: "raw body serialization",
			mutate: func(observation *DifferentialObservation) {
				observation.Upstream.Body = `{"TextSegments":[{"Text":"toxic"}],"LanguageCode":"en"}`
			},
		},
		{
			name: "captured semantic header",
			mutate: func(observation *DifferentialObservation) {
				observation.Upstream.Headers["X-Consumer-Username"] = []string{"alice"}
			},
		},
		{
			name: "response header",
			mutate: func(observation *DifferentialObservation) {
				observation.Headers["X-Plugin-Result"] = []string{"different"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, candidate, oracle := differentialAIAWSComprehendSigV4TestObservations()
			test.mutate(&oracle)
			equal, _, err := compareDifferentialAIAWSComprehendSigV4(
				spec,
				candidate,
				oracle,
				testNormalizationPolicy(),
			)
			if err != nil {
				t.Fatalf("compare strict %s error = %v", test.name, err)
			}
			if equal {
				t.Fatalf("%s difference was normalized", test.name)
			}
		})
	}
}

func TestCompareDifferentialFixtureOwnedFunctionEndpoint(t *testing.T) {
	spec, candidate, oracle := differentialFixtureOwnedFunctionEndpointTestObservations()
	candidateBefore := cloneDifferentialObservation(candidate)
	oracleBefore := cloneDifferentialObservation(oracle)

	equal, detail, err := compareDifferentialFixtureOwnedFunctionEndpoint(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil || !equal || detail != "" {
		t.Fatalf("compare pinned function observations = %t, %q, %v", equal, detail, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("function endpoint comparison mutated caller observations")
	}
}

func TestCompareDifferentialFixtureOwnedFunctionEndpointRejectsMalformedContract(t *testing.T) {
	assertError := func(
		t *testing.T,
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation),
		want string,
	) {
		t.Helper()
		spec, candidate, oracle := differentialFixtureOwnedFunctionEndpointTestObservations()
		mutate(&spec, &candidate, &oracle)
		equal, _, err := compareDifferentialFixtureOwnedFunctionEndpoint(
			spec,
			candidate,
			oracle,
			testNormalizationPolicy(),
		)
		if err == nil || equal || !strings.Contains(err.Error(), want) {
			t.Fatalf("compare malformed contract = %t, %v, want error containing %q", equal, err, want)
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned spec",
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Request.Path = "/other"
			},
			want: "pinned",
		},
		{
			name: "wrong response status",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Status = http.StatusCreated
			},
			want: "200 function response",
		},
		{
			name: "wrong response body",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Body = "other"
			},
			want: "200 function response",
		},
		{
			name: "wrong security decision",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.SecurityDecision = "deny"
			},
			want: "security decision",
		},
		{
			name: "missing fixture request",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Upstream.Received = false
			},
			want: "one identified candidate fixture request",
		},
		{
			name: "fixture identity mismatch",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Upstream.Fixture = "secondary"
			},
			want: "one identified oracle fixture request",
		},
		{
			name: "recorded retry",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.RetryCount = 1
			},
			want: "exactly one candidate fixture request",
		},
		{
			name: "sequence call mixed into single request",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls = []DifferentialUpstreamObservation{oracle.Upstream}
			},
			want: "exactly one oracle fixture request",
		},
		{
			name: "fixture Host does not equal address",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.Upstream.Host = "lambda.example.test"
			},
			want: "upstream Host",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertError(t, test.mutate, test.want)
		})
	}
}

func TestCompareDifferentialFixtureOwnedFunctionEndpointMapsOnlyHost(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialObservation)
	}{
		{
			name: "method",
			mutate: func(observation *DifferentialObservation) {
				observation.Upstream.Method = http.MethodPost
			},
		},
		{
			name: "path",
			mutate: func(observation *DifferentialObservation) {
				observation.Upstream.Path = "/other"
			},
		},
		{
			name: "query",
			mutate: func(observation *DifferentialObservation) {
				observation.Upstream.Path = "/httptrigger?source=oracle"
			},
		},
		{
			name: "body",
			mutate: func(observation *DifferentialObservation) {
				observation.Upstream.Body = "payload"
			},
		},
		{
			name: "request header",
			mutate: func(observation *DifferentialObservation) {
				observation.Upstream.Headers = map[string][]string{"Authorization": {"Bearer token"}}
			},
		},
		{
			name: "response header",
			mutate: func(observation *DifferentialObservation) {
				observation.Headers["X-Function"] = []string{"different"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, candidate, oracle := differentialFixtureOwnedFunctionEndpointTestObservations()
			test.mutate(&oracle)
			equal, _, err := compareDifferentialFixtureOwnedFunctionEndpoint(
				spec,
				candidate,
				oracle,
				testNormalizationPolicy(),
			)
			if err == nil && equal {
				t.Fatalf("%s difference was normalized", test.name)
			}
		})
	}
}

func differentialAIAWSComprehendSigV4TestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	spec := differentialCasesForPlugin("ai-aws-content-moderation")[0]
	observation := func(address, path, contentType, authorization string) DifferentialObservation {
		return DifferentialObservation{
			Status:           http.StatusBadRequest,
			Headers:          map[string][]string{"Content-Type": {contentType}},
			Body:             "request body exceeds toxicity threshold",
			UpstreamFixture:  "primary",
			UpstreamAddress:  address,
			Host:             spec.Request.Host,
			SecurityDecision: "deny",
			Upstream: DifferentialUpstreamObservation{
				Received: true,
				Fixture:  "primary",
				Method:   http.MethodPost,
				Path:     path,
				Host:     address,
				Headers:  map[string][]string{"Authorization": {authorization}},
				Body:     `{"LanguageCode":"en","TextSegments":[{"Text":"toxic"}]}`,
			},
		}
	}
	candidate := observation(
		"127.0.0.1:31001",
		"/",
		"text/plain",
		"AWS4-HMAC-SHA256 Credential=access/20260828/us-east-1/comprehend/aws4_request, "+
			"SignedHeaders="+differentialAWSCandidateSignedHeaders+", Signature="+strings.Repeat("a", 64),
	)
	oracle := observation(
		"127.0.0.1:1980",
		"/?",
		"text/plain; charset=utf-8",
		"AWS4-HMAC-SHA256 Credential=access/20260829/us-east-1/comprehend/aws4_request, "+
			"SignedHeaders="+differentialAWSOracleSignedHeaders+", Signature="+strings.Repeat("b", 64),
	)
	return spec, candidate, oracle
}

func setDifferentialAWSAuthorization(observation *DifferentialObservation, value string) {
	observation.Upstream.Headers = map[string][]string{"Authorization": {value}}
}

func differentialFixtureOwnedFunctionEndpointTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	spec := differentialCasesForPlugin("aws-lambda")[0]
	observation := func(address string) DifferentialObservation {
		return DifferentialObservation{
			Status:           http.StatusOK,
			Headers:          map[string][]string{"Content-Type": {"text/plain"}},
			Body:             "aws lambda invoked",
			UpstreamFixture:  "primary",
			UpstreamAddress:  address,
			Host:             spec.Request.Host,
			SecurityDecision: "allow",
			Upstream: DifferentialUpstreamObservation{
				Received: true,
				Fixture:  "primary",
				Method:   http.MethodGet,
				Path:     "/httptrigger?",
				Host:     strings.Split(address, ":")[0],
			},
		}
	}
	return spec, observation("127.0.0.1:31002"), observation("127.0.0.1:1980")
}
