package pluginintegration

import "net/http"

const (
	differentialDatadogSixDatagramsPolicy  = "datadog-six-ordered-dogstatsd-datagrams"
	differentialDatadogHTTPUDPWireProtocol = "http-udp"
	differentialDatadogRouteID             = "differential-datadog-six-datagrams"
	differentialDatadogGatewayPath         = "/opentracing"
)

// differentialDatadogCases maps APISIX 3.17 t/plugin/datadog.t TEST 2/3.
// The shared numeric port serves the HTTP origin over TCP and captures the
// DogStatsD sink over UDP. Only the six UDP datagrams become fixture calls.
func differentialDatadogCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:             "datadog-emits-six-ordered-single-metric-datagrams",
		Plugin:           "datadog",
		RouteID:          differentialDatadogRouteID,
		ComparisonPolicy: differentialDatadogSixDatagramsPolicy,
		Config: map[string]any{
			"plugin_metadata": []any{map[string]any{
				"id": "datadog", "host": differentialFixtureHostPlaceholder,
				"port": differentialFixturePortPlaceholder,
			}},
			"routes": []any{map[string]any{
				"id": differentialDatadogRouteID, "name": "datadog",
				"uri": differentialDatadogGatewayPath,
				"plugins": map[string]any{"datadog": map[string]any{
					"batch_max_size": 1, "max_retry_count": 0, "retry_delay": 0,
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: differentialDatadogGatewayPath,
				Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name:                 "datadog-http-origin-and-udp-sink",
			WireProtocol:         differentialDatadogHTTPUDPWireProtocol,
			ExpectedCalls:        6,
			CaptureAllCalls:      true,
			OmitHTTPOriginCall:   true,
			CollectTimeoutMillis: 5000,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK, Headers: map[string]string{"Content-Type": "text/plain"},
				Body: "opentracing",
			},
		},
	}}
}
