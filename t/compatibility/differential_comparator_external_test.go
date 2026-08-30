package pluginintegration

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestDifferentialExternalComparatorsAcceptOnlyReviewedVolatility(t *testing.T) {
	tests := []struct {
		name    string
		fixture func() (DifferentialCase, DifferentialObservation, DifferentialObservation)
		compare differentialComparator
	}{
		{
			name:    "azure functions",
			fixture: differentialAzureFunctionsComparatorTestObservations,
			compare: compareDifferentialAzureFunctionsFixtureInvocation,
		},
		{
			name:    "OPA",
			fixture: differentialOPAComparatorTestObservations,
			compare: compareDifferentialOPAFixtureDecision,
		},
		{
			name:    "DingTalk",
			fixture: differentialDingTalkComparatorTestObservations,
			compare: compareDifferentialDingTalkAuthFixtureOAuth,
		},
		{
			name:    "Feishu",
			fixture: differentialFeishuComparatorTestObservations,
			compare: compareDifferentialFeishuAuthFixtureOAuth,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, candidate, oracle := test.fixture()
			candidateBefore := cloneDifferentialObservation(candidate)
			oracleBefore := cloneDifferentialObservation(oracle)

			equal, detail, err := test.compare(
				spec,
				candidate,
				oracle,
				testNormalizationPolicy(),
			)
			if err != nil || !equal || detail != "" {
				t.Fatalf("compare reviewed observations = %t, %q, %v", equal, detail, err)
			}
			if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
				t.Fatal("comparison mutated caller observations")
			}
		})
	}
}

func TestDifferentialExternalComparatorsRejectUnpinnedCases(t *testing.T) {
	tests := []struct {
		name    string
		fixture func() (DifferentialCase, DifferentialObservation, DifferentialObservation)
		compare differentialComparator
	}{
		{
			"azure functions",
			differentialAzureFunctionsComparatorTestObservations,
			compareDifferentialAzureFunctionsFixtureInvocation,
		},
		{"OPA", differentialOPAComparatorTestObservations, compareDifferentialOPAFixtureDecision},
		{"DingTalk", differentialDingTalkComparatorTestObservations, compareDifferentialDingTalkAuthFixtureOAuth},
		{"Feishu", differentialFeishuComparatorTestObservations, compareDifferentialFeishuAuthFixtureOAuth},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, candidate, oracle := test.fixture()
			spec.Request.Path = "/different"
			equal, _, err := test.compare(spec, candidate, oracle, testNormalizationPolicy())
			if err == nil || equal || !strings.Contains(err.Error(), "exact pinned") {
				t.Fatalf("compare unpinned case = %t, %v", equal, err)
			}
		})
	}
}

func TestDifferentialAzureFunctionsComparatorRejectsSemanticChanges(t *testing.T) {
	mutations := []struct {
		name string
		edit func(*DifferentialObservation)
	}{
		{"response body", func(got *DifferentialObservation) { got.Body = "wrong" }},
		{"function key", func(got *DifferentialObservation) {
			got.Upstream.Headers["X-Functions-Key"] = []string{"wrong"}
		}},
		{"function path", func(got *DifferentialObservation) { got.Upstream.Path = "/other?" }},
		{"extra semantic header", func(got *DifferentialObservation) {
			got.Upstream.Headers["X-Functions-Clientid"] = []string{"unexpected"}
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			spec, candidate, oracle := differentialAzureFunctionsComparatorTestObservations()
			test.edit(&oracle)
			equal, _, err := compareDifferentialAzureFunctionsFixtureInvocation(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil && equal {
				t.Fatal("semantic difference was normalized")
			}
		})
	}
}

func TestDifferentialOPAComparatorKeepsStaticAndUnknownFieldsStrict(t *testing.T) {
	t.Run("unknown request field", func(t *testing.T) {
		spec, candidate, oracle := differentialOPAComparatorTestObservations()
		oracle.Upstream.Body = strings.Replace(
			oracle.Upstream.Body,
			`"path":"/test",`,
			`"path":"/test","credential":"must-not-be-ignored",`,
			1,
		)
		equal, _, err := compareDifferentialOPAFixtureDecision(
			spec, candidate, oracle, testNormalizationPolicy(),
		)
		if err == nil || equal || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("compare unknown OPA field = %t, %v", equal, err)
		}
	})

	t.Run("version difference", func(t *testing.T) {
		spec, candidate, oracle := differentialOPAComparatorTestObservations()
		candidate.Upstream.Body = strings.Replace(
			candidate.Upstream.Body,
			`"input":{`,
			`"input":{"version":1,`,
			1,
		)
		equal, _, err := compareDifferentialOPAFixtureDecision(
			spec, candidate, oracle, testNormalizationPolicy(),
		)
		if err != nil {
			t.Fatalf("compare optional reviewed version field: %v", err)
		}
		if equal {
			t.Fatal("static OPA version difference was normalized")
		}
	})

	t.Run("policy request", func(t *testing.T) {
		spec, candidate, oracle := differentialOPAComparatorTestObservations()
		oracle.Upstream.Body = strings.Replace(oracle.Upstream.Body, `"user":"carla"`, `"user":"mallory"`, 1)
		equal, _, err := compareDifferentialOPAFixtureDecision(
			spec, candidate, oracle, testNormalizationPolicy(),
		)
		if err == nil && equal {
			t.Fatal("OPA query difference was normalized")
		}
	})
}

func TestDifferentialOAuthComparatorsRejectStaticCallAndCookieChanges(t *testing.T) {
	tests := []struct {
		name    string
		fixture func() (DifferentialCase, DifferentialObservation, DifferentialObservation)
		compare differentialComparator
	}{
		{"DingTalk", differentialDingTalkComparatorTestObservations, compareDifferentialDingTalkAuthFixtureOAuth},
		{"Feishu", differentialFeishuComparatorTestObservations, compareDifferentialFeishuAuthFixtureOAuth},
	}

	for _, test := range tests {
		t.Run(test.name+" provider body", func(t *testing.T) {
			spec, candidate, oracle := test.fixture()
			oracle.UpstreamCalls[0].Body = `{"unexpected":true}`
			equal, _, err := test.compare(spec, candidate, oracle, testNormalizationPolicy())
			if err == nil && equal {
				t.Fatal("provider body difference was normalized")
			}
		})
		t.Run(test.name+" upstream userinfo", func(t *testing.T) {
			spec, candidate, oracle := test.fixture()
			oracle.UpstreamCalls[2].Headers["X-Userinfo"] = []string{"bm90LWpzb24="}
			oracle.Upstream = oracle.UpstreamCalls[2]
			equal, _, err := test.compare(spec, candidate, oracle, testNormalizationPolicy())
			if err == nil && equal {
				t.Fatal("userinfo difference was normalized")
			}
		})
		t.Run(test.name+" cookie path", func(t *testing.T) {
			spec, candidate, oracle := test.fixture()
			oracle.Headers["Set-Cookie"] = []string{
				strings.Replace(oracle.Headers["Set-Cookie"][0], "Path=/", "Path=/other", 1),
			}
			equal, _, err := test.compare(spec, candidate, oracle, testNormalizationPolicy())
			if err == nil && equal {
				t.Fatal("static cookie Path difference was normalized")
			}
		})
	}
}

func differentialAzureFunctionsComparatorTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	spec := differentialCasesForPlugin("azure-functions")[0]
	observation := func(address string) DifferentialObservation {
		host := strings.Split(address, ":")[0]
		return DifferentialObservation{
			Status: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type":   {"text/plain; charset=utf-8"},
				"X-Extra-Header": {"MUST"},
			},
			Body:             "faas invoked",
			UpstreamFixture:  "primary",
			UpstreamAddress:  address,
			Host:             spec.Request.Host,
			SecurityDecision: "allow",
			Upstream: DifferentialUpstreamObservation{
				Received: true,
				Fixture:  "primary",
				Method:   http.MethodGet,
				Path:     "/httptrigger?",
				Host:     host,
				Headers:  map[string][]string{"X-Functions-Key": {"test_key"}},
			},
		}
	}
	return spec, observation("127.0.0.1:31011"), observation("127.0.0.1:1980")
}

func differentialOPAComparatorTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	spec := differentialCasesForPlugin("opa")[0]
	body := func(serverPort, remotePort, timestamp string, connection bool) string {
		headers := fmt.Sprintf(
			`"host":"gateway.example.test","test-header":"only-for-test","user-agent":"Go-http-client/1.1","x-forwarded-host":"gateway.example.test","x-forwarded-port":"%s","x-forwarded-proto":"http"`,
			serverPort,
		)
		if connection {
			headers += `,"connection":"close"`
		}
		return fmt.Sprintf(
			`{"input":{"type":"http","request":{"scheme":"http","method":"GET","host":"gateway.example.test","port":%s,"path":"/test","headers":{%s},"query":{"test":"abcd","user":"carla"}},"var":{"server_addr":"127.0.0.1","server_port":"%s","remote_addr":"127.0.0.1","remote_port":"%s","timestamp":%s}}}`,
			serverPort,
			headers,
			serverPort,
			remotePort,
			timestamp,
		)
	}
	observation := func(address, requestBody string) DifferentialObservation {
		return DifferentialObservation{
			Status:           http.StatusForbidden,
			Body:             "Give you a string reason",
			UpstreamFixture:  "primary",
			UpstreamAddress:  address,
			Host:             spec.Request.Host,
			SecurityDecision: "deny",
			Upstream: DifferentialUpstreamObservation{
				Received: true,
				Fixture:  "primary",
				Method:   http.MethodPost,
				Path:     "/v1/data/example",
				Host:     address,
				Headers:  map[string][]string{"Content-Type": {"application/json"}},
				Body:     requestBody,
			},
		}
	}
	return spec,
		observation("127.0.0.1:31012", body("31080", "51111", "1787900000", false)),
		observation("127.0.0.1:1980", body("9080", "52222", "1787900001", true))
}

func differentialDingTalkComparatorTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	spec := differentialCasesForPlugin("dingtalk-auth")[0]
	return spec,
		differentialOAuthComparatorObservation(
			spec,
			"127.0.0.1:31013",
			"dingtalk_session",
			"candidate-token",
			"Tue, 28 Aug 2026 10:00:00 GMT",
			[]DifferentialUpstreamObservation{
				differentialOAuthCall(
					"primary",
					"127.0.0.1:31013",
					http.MethodPost,
					"/v1.0/oauth2/accessToken",
					map[string][]string{"Content-Type": {"application/json"}},
					`{"appKey":"testappkey","appSecret":"testappsecret"}`,
				),
				differentialOAuthCall(
					"primary",
					"127.0.0.1:31013",
					http.MethodPost,
					"/topapi/v2/user/getuserinfo?access_token=access-token-a",
					map[string][]string{"Content-Type": {"application/json"}},
					`{"code":"valid_code"}`,
				),
				differentialOAuthCall(
					"primary",
					"differential.example.test",
					http.MethodGet,
					"/hello",
					map[string][]string{"X-Userinfo": {"eyJuYW1lIjoiQWxpY2UiLCJ1c2VyaWQiOiJ1c2VyLWEifQ=="}},
					"",
				),
			},
		),
		differentialOAuthComparatorObservation(
			spec,
			"127.0.0.1:1980",
			"dingtalk_session",
			"oracle-token",
			"Tue, 28 Aug 2026 10:00:01 GMT",
			[]DifferentialUpstreamObservation{
				differentialOAuthCall(
					"primary",
					"127.0.0.1:1980",
					http.MethodPost,
					"/v1.0/oauth2/accessToken?",
					map[string][]string{"content-type": {"application/json"}},
					`{"appSecret":"testappsecret","appKey":"testappkey"}`,
				),
				differentialOAuthCall(
					"primary",
					"127.0.0.1:1980",
					http.MethodPost,
					"/topapi/v2/user/getuserinfo?access_token=access-token-a",
					map[string][]string{"content-type": {"application/json"}},
					`{"code":"valid_code"}`,
				),
				differentialOAuthCall(
					"primary",
					"differential.example.test",
					http.MethodGet,
					"/hello?",
					map[string][]string{"x-userinfo": {"eyJ1c2VyaWQiOiJ1c2VyLWEiLCJuYW1lIjoiQWxpY2UifQ=="}},
					"",
				),
			},
		)
}

func differentialFeishuComparatorTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	spec := differentialCasesForPlugin("feishu-auth")[0]
	return spec,
		differentialOAuthComparatorObservation(
			spec,
			"127.0.0.1:31014",
			"feishu_session",
			"candidate-token",
			"Tue, 28 Aug 2026 10:00:00 GMT",
			[]DifferentialUpstreamObservation{
				differentialOAuthCall(
					"primary",
					"127.0.0.1:31014",
					http.MethodPost,
					"/token",
					map[string][]string{"Content-Type": {"application/json"}},
					`{"client_id":"123","client_secret":"456","code":"passed","grant_type":"authorization_code","redirect_uri":"https://example.com/callback"}`,
				),
				differentialOAuthCall(
					"primary",
					"127.0.0.1:31014",
					http.MethodGet,
					"/userinfo",
					map[string][]string{
						"Authorization": {"Bearer access-token-a"},
						"Content-Type":  {"application/json"},
					},
					"",
				),
				differentialOAuthCall(
					"primary",
					"differential.example.test",
					http.MethodGet,
					"/hello",
					map[string][]string{"X-Userinfo": {"eyJuYW1lIjoiQWxpY2UiLCJvcGVuX2lkIjoib3UtYSJ9"}},
					"",
				),
			},
		),
		differentialOAuthComparatorObservation(
			spec,
			"127.0.0.1:1980",
			"feishu_session",
			"oracle-token",
			"Tue, 28 Aug 2026 10:00:01 GMT",
			[]DifferentialUpstreamObservation{
				differentialOAuthCall(
					"primary",
					"127.0.0.1:1980",
					http.MethodPost,
					"/token?",
					map[string][]string{"content-type": {"application/json"}},
					`{"grant_type":"authorization_code","client_id":"123","client_secret":"456","redirect_uri":"https://example.com/callback","code":"passed"}`,
				),
				differentialOAuthCall(
					"primary",
					"127.0.0.1:1980",
					http.MethodGet,
					"/userinfo?",
					map[string][]string{
						"authorization": {"Bearer access-token-a"},
						"content-type":  {"application/json"},
					},
					"",
				),
				differentialOAuthCall(
					"primary",
					"differential.example.test",
					http.MethodGet,
					"/hello?",
					map[string][]string{"x-userinfo": {"eyJvcGVuX2lkIjoib3UtYSIsIm5hbWUiOiJBbGljZSJ9"}},
					"",
				),
			},
		)
}

func differentialOAuthComparatorObservation(
	spec DifferentialCase,
	address string,
	cookieName string,
	cookieValue string,
	expires string,
	calls []DifferentialUpstreamObservation,
) DifferentialObservation {
	return DifferentialObservation{
		Status: http.StatusOK,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"Set-Cookie": {
				fmt.Sprintf(
					"%s=%s; Path=/; Expires=%s; Max-Age=86400; HttpOnly; Secure; SameSite=Lax",
					cookieName,
					cookieValue,
					expires,
				),
			},
		},
		Body:             spec.Fixture.Response.Body,
		UpstreamFixture:  "primary",
		UpstreamAddress:  address,
		Host:             spec.Request.Host,
		SecurityDecision: "allow",
		Upstream:         calls[len(calls)-1],
		UpstreamCalls:    calls,
	}
}

func differentialOAuthCall(
	fixture string,
	host string,
	method string,
	path string,
	headers map[string][]string,
	body string,
) DifferentialUpstreamObservation {
	return DifferentialUpstreamObservation{
		Received: true,
		Fixture:  fixture,
		Method:   method,
		Path:     path,
		Host:     host,
		Headers:  headers,
		Body:     body,
	}
}
