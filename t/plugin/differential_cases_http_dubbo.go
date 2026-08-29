package pluginintegration

import "net/http"

// differentialHTTPDubboCases maps APISIX 3.17 t/plugin/http-dubbo.t TEST 1
// to one real Dubbo 2.x FastJSON request/response exchange.
func differentialHTTPDubboCases() []DifferentialCase {
	const routeID = "differential-http-dubbo-pojo-fastjson"
	return []DifferentialCase{{
		Name:             "http-dubbo-pojo-fastjson",
		Plugin:           "http-dubbo",
		RouteID:          routeID,
		ComparisonPolicy: differentialHTTPDubboPOJOPolicy,
		Config: map[string]any{"routes": []any{map[string]any{
			"id": routeID, "uri": "/t",
			"plugins": map[string]any{"http-dubbo": map[string]any{
				"service_name":     differentialHTTPDubboServiceName,
				"service_version":  differentialHTTPDubboServiceVersion,
				"method":           differentialHTTPDubboMethodName,
				"params_type_desc": differentialHTTPDubboParamsTypeDesc,
				"serialized":       true,
			}},
			"upstream": map[string]any{
				"type": "roundrobin", "nodes": map[string]any{differentialFixturePlaceholder: 1},
			},
		}}},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/t", Host: "gateway.example.test",
			Body: differentialHTTPDubboPOJOJSON,
		},
		Fixture: DifferentialFixture{
			Name: "http-dubbo", WireProtocol: differentialFixtureWireHTTPDubboFastJSON,
			ExpectedCalls: 1, SemanticHeaders: []string{differentialHTTPDubboParamsTypeHeader},
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: differentialHTTPDubboPOJOJSON},
		},
		SecurityDecision: "not_applicable",
	}}
}
