package pluginintegration

import "net/http"

const (
	differentialSLSLoggerTLSDeliveryPolicy = "sls-logger-rfc5424-tls-delivery"
	differentialSLSLoggerGatewayPath       = "/logger/sls"
	differentialSLSLoggerProject           = "differential-project"
	differentialSLSLoggerLogstore          = "differential-logstore"
	differentialSLSLoggerAccessKeyID       = "differential-access-key-id"
	differentialSLSLoggerAccessKeySecret   = "differential-access-key-secret"
)

// differentialSLSLoggerCases derives one source-parity scenario from APISIX
// 3.17 t/plugin/sls-logger.t TEST 4/5, TEST 6/7, and TEST 12/13 plus the pinned
// sls-logger and RFC5424 implementations. It combines a mocked gateway response
// with one raw TLS TCP frame. APISIX 3.17 performs this TLS handshake without
// peer verification, so the config deliberately has no Go-only ssl_verify field.
func differentialSLSLoggerCases() []DifferentialCase {
	const routeID = "differential-sls-logger-tls"

	return []DifferentialCase{{
		Name:             "sls-logger-sends-single-rfc5424-frame-over-tls",
		Plugin:           "sls-logger",
		RouteID:          routeID,
		ComparisonPolicy: differentialSLSLoggerTLSDeliveryPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": differentialSLSLoggerGatewayPath,
				"plugins": map[string]any{
					"sls-logger": map[string]any{
						"host":              differentialFixtureHostPlaceholder,
						"port":              differentialFixturePortPlaceholder,
						"project":           differentialSLSLoggerProject,
						"logstore":          differentialSLSLoggerLogstore,
						"access_key_id":     differentialSLSLoggerAccessKeyID,
						"access_key_secret": differentialSLSLoggerAccessKeySecret,
						"log_format":        map[string]any{"case": "sls-logger"},
						"timeout":           3000, "batch_max_size": 1,
						"inactive_timeout": 1, "max_retry_count": 0,
					},
					"mocking": map[string]any{
						"content_type": "text/plain", "response_status": http.StatusOK,
						"response_example": "fixture-ok", "with_mock_header": false,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: differentialSLSLoggerGatewayPath,
				Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "sls-logger-tls", WireProtocol: "tls-tcp",
			ExpectedCalls: 1, CollectTimeoutMillis: 6000,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "fixture-ok"},
		},
	}}
}
