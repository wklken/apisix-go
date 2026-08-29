package pluginintegration

import "net/http"

const differentialServerInfoControlAPIPolicy = "server-info-control-api"

func differentialServerInfoCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:             "server-info-control-api-shape",
		Plugin:           "server-info",
		RouteID:          "differential-server-info-control",
		ComparisonPolicy: differentialServerInfoControlAPIPolicy,
		Config:           map[string]any{"routes": []any{}},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/v1/server_info", Host: "gateway.example.test",
			Target: DifferentialRequestTargetControl,
		},
		Fixture: DifferentialFixture{
			Name: "unused", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unexpected"},
		},
		SecurityDecision: "not_applicable",
	}}
}
