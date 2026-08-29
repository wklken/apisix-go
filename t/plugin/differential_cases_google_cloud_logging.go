package pluginintegration

import "net/http"

const (
	differentialGoogleCloudLoggingFixtureDeliveryPolicy = "google-cloud-logging-oauth-entry-delivery"
	differentialGoogleCloudLoggingRouteID               = "differential-google-cloud-logging"
	differentialGoogleCloudLoggingGatewayPath           = "/google-cloud-logging"
	differentialGoogleCloudLoggingTokenPath             = "/token"
	differentialGoogleCloudLoggingEntriesPath           = "/entries"
	differentialGoogleCloudLoggingClientEmail           = "differential@example.iam.gserviceaccount.com"
	differentialGoogleCloudLoggingProjectID             = "differential-project"
	differentialGoogleCloudLoggingScope                 = "https://apisix.apache.org/logs:admin"
	differentialGoogleCloudLoggingLogID                 = "differential-log"
	differentialGoogleCloudLoggingJWTBearerGrantType    = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	differentialGoogleCloudLoggingTokenResponse         = `{"access_token":"differential-access-token","token_type":"Bearer","expires_in":3600}`

	differentialGoogleCloudLoggingPrivateKey = `-----BEGIN PRIVATE KEY-----
MIICdQIBADANBgkqhkiG9w0BAQEFAASCAl8wggJbAgEAAoGBAKelw5sMmKEiq8tf
ZkXFcsAwfYASttNm1kDDRBf2LlM6F+AqovgCbtgDCG2CR6d6ZSc2VI2KwGqVmz04
xTD5G8ynPp0nantvG8wBIbPNFJxVxqTAdNPcPHN7idQ2Y8wifj6eDljpPyIDhd3k
blABvc7ZqP9E9Cv/PWyfVPW5IsZTAgMBAAECgYAu2SnCSFDWpqOvX2drE/QvNN29
Tn18sf4pdueucoMbit5lLEUCXVuwTZirUX7IlHFz9cDHFQEUR95ry1N/jf1wTXF0
q9dMK1sOxeIxD5u87IT/YUFOGR37PpfzBqr4lXBqSFHOr3pnMyboyYzl/hsR5QkC
ytxBICCR17uQAU1z0QJBANSScivNYrzQA+jLrnUUMerY3VJUy7OelC7aOvM+w0Wg
g2y7wLWcG0vQpJEeLh6xXPxu/98Q57gDkUjEu4G8c30CQQDJ5cHY63WIHr3SX+6s
cqO5tklJKB7TYBxZXJKLRewW6xkzcjMXlLsZzn9oFtr5e6ggcyvvViQmjH9UBxNC
s6oPAkApUp6nLTH4imd4JcAwOlDJ2oaLrrg6nqUnxnyXNKg5LM7foFAB/erAfjq/
iyJkDQ6Kc/mBn4OsHeVsQ/I/cibxAkBfHMf3guU5nRHbu6nav5717DQWLLpo5cw1
JPE8f1I7ccHLhK8hGsYR4EARL0M1aNXJg7hc5f3d0y5gzXx7XdxtAkATXODoxB8h
KbxxbtNMlYkGCrYKfZgiZWstfYYAOliciGSqmHE2eqED/424yuTO9m4PIyKXKWwS
JKuSmg0k5oxG
-----END PRIVATE KEY-----`
)

// differentialGoogleCloudLoggingCases maps APISIX 3.17
// t/plugin/google-cloud-logging.t TEST 2 and TEST 10/11 to one local HTTP
// fixture. It proves the service-account JWT exchange and Logging entries
// request shape; it intentionally makes no claim about the public GCP service.
func differentialGoogleCloudLoggingCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:             "google-cloud-logging-exchanges-jwt-and-writes-custom-entry",
		Plugin:           "google-cloud-logging",
		RouteID:          differentialGoogleCloudLoggingRouteID,
		ComparisonPolicy: differentialGoogleCloudLoggingFixtureDeliveryPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  differentialGoogleCloudLoggingRouteID,
				"uri": differentialGoogleCloudLoggingGatewayPath,
				"plugins": map[string]any{
					"google-cloud-logging": map[string]any{
						"auth_config": map[string]any{
							"client_email": differentialGoogleCloudLoggingClientEmail,
							"private_key":  differentialGoogleCloudLoggingPrivateKey,
							"project_id":   differentialGoogleCloudLoggingProjectID,
							"token_uri": "http://" + differentialFixturePlaceholder +
								differentialGoogleCloudLoggingTokenPath,
							"entries_uri": "http://" + differentialFixturePlaceholder +
								differentialGoogleCloudLoggingEntriesPath,
							"scope": []any{differentialGoogleCloudLoggingScope},
						},
						"resource": map[string]any{
							"type": "global",
							"labels": map[string]any{
								"project_id": differentialGoogleCloudLoggingProjectID,
							},
						},
						"log_id": differentialGoogleCloudLoggingLogID,
						"log_format": map[string]any{
							"case":     "google-cloud-logging",
							"route_id": "$route_id",
						},
						"batch_max_size":   1,
						"inactive_timeout": 1,
						"max_retry_count":  0,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   differentialGoogleCloudLoggingGatewayPath,
				Host:   "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name:                 "origin-and-google-cloud-logging",
			ExpectedCalls:        3,
			CaptureAllCalls:      true,
			CollectTimeoutMillis: 7000,
			SemanticHeaders:      []string{"Authorization", "Content-Type"},
			Response: DifferentialFixtureResponse{
				Status:  http.StatusOK,
				Headers: map[string]string{"Content-Type": "application/json"},
				Body:    differentialGoogleCloudLoggingTokenResponse,
			},
		},
	}}
}
