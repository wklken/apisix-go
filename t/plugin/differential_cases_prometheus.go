package pluginintegration

import "net/http"

const differentialComparisonPrometheusRouteStatusSeries = "prometheus-route-status-series"

// differentialPrometheusCases maps APISIX 3.17 t/plugin/prometheus.t
// TEST 2/4/6: expose the metrics API, proxy one request through a route with
// prometheus enabled, then scrape the resulting route-labelled metric series.
func differentialPrometheusCases() []DifferentialCase {
	const routeID = "differential-prometheus-route"

	return []DifferentialCase{{
		Name:             "prometheus-records-route-status-series",
		Plugin:           "prometheus",
		RouteID:          routeID,
		ComparisonPolicy: differentialComparisonPrometheusRouteStatusSeries,
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":      "differential-prometheus-public-api",
					"uri":     "/apisix/prometheus/metrics",
					"methods": []any{http.MethodGet},
					"plugins": map[string]any{
						"public-api": map[string]any{},
					},
				},
				map[string]any{
					"id":  routeID,
					"uri": "/profile",
					"plugins": map[string]any{
						"prometheus": map[string]any{},
					},
					"upstream": differentialUpstream(),
				},
			},
		},
		Steps: []DifferentialStep{
			{
				Request: DifferentialRequest{
					Method: http.MethodGet,
					Path:   "/profile",
					Host:   "gateway.example.test",
				},
				SecurityDecision: "not_applicable",
			},
			{
				DelayBeforeMillis: 1500,
				Request: DifferentialRequest{
					Method: http.MethodGet,
					Path:   "/apisix/prometheus/metrics",
					Host:   "gateway.example.test",
				},
				SecurityDecision: "not_applicable",
			},
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "profile-ok",
			},
		},
	}}
}
