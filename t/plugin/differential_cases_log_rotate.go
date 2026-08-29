package pluginintegration

import "net/http"

// differentialLogRotateCases maps the observable file behavior from APISIX
// 3.17 t/plugin/log-rotate2.t TEST 1/2/5 and log-rotate3.t TEST 1/2. The size
// trigger avoids using elapsed wall time as the differential assertion.
func differentialLogRotateCases() []DifferentialCase {
	const routeID = "differential-log-rotate-size-compress-prune-reopen"
	step := func(path string) DifferentialStep {
		return DifferentialStep{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: path, Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}
	}
	return []DifferentialCase{{
		Name:             "log-rotate-size-compress-prune-reopen",
		Plugin:           "log-rotate",
		RouteID:          routeID,
		ComparisonPolicy: differentialLogRotatePolicy,
		Config: map[string]any{"routes": []any{map[string]any{
			"id": routeID, "uri": "/*",
			"plugins": map[string]any{"file-logger": map[string]any{
				"path":       differentialLogRotateSideDirectoryPlaceholder + "/logs/access.log",
				"log_format": map[string]any{"path": "$uri"},
			}},
			"upstream": differentialUpstream(),
		}}},
		Steps: []DifferentialStep{step("/rotate"), step("/after-rotate")},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 2, CaptureAllCalls: true,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "ok"},
		},
		SecurityDecision: "not_applicable",
	}}
}
