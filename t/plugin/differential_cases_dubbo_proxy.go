package pluginintegration

import "net/http"

const (
	differentialDubboProxyHessian2Policy  = "dubbo-proxy-hessian2-http-context"
	differentialDubboProxyProtocolVersion = "2.0.2"
	differentialDubboProxyServiceName     = "org.apache.dubbo.backend.DemoService"
	differentialDubboProxyServiceVersion  = "1.0.0"
	differentialDubboProxyMethodName      = "hello"
	differentialDubboProxyParamsTypeDesc  = "Ljava/util/Map;"
	differentialDubboProxyHTTPHost        = "gateway.example.test"
	differentialDubboProxyHeaderValue     = "V"
	differentialDubboProxyRequestBody     = "request body"
	differentialDubboProxyResponseBody    = "dubbo success\n"
)

// differentialDubboProxyCases maps APISIX 3.17
// t/plugin/dubbo-proxy/route.t TEST 3 to one real Hessian2 exchange. The
// request adds a body and explicit Host so the same Map<String,Object>
// transport contract is observed rather than inferred from the source's
// header-only request.
func differentialDubboProxyCases() []DifferentialCase {
	const routeID = "differential-dubbo-proxy-hessian2-http-context"
	return []DifferentialCase{{
		Name:             "dubbo-proxy-hessian2-http-context",
		Plugin:           "dubbo-proxy",
		RouteID:          routeID,
		ComparisonPolicy: differentialDubboProxyHessian2Policy,
		Config: map[string]any{"routes": []any{map[string]any{
			"id": routeID, "uri": "/hello",
			"plugins": map[string]any{"dubbo-proxy": map[string]any{
				"service_name":    differentialDubboProxyServiceName,
				"service_version": differentialDubboProxyServiceVersion,
				"method":          differentialDubboProxyMethodName,
			}},
			"upstream": map[string]any{
				"type": "roundrobin", "nodes": map[string]any{differentialFixturePlaceholder: 1},
			},
		}}},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/hello", Host: differentialDubboProxyHTTPHost,
			Headers: map[string]string{"Extra-Arg-K": differentialDubboProxyHeaderValue},
			Body:    differentialDubboProxyRequestBody,
		},
		Fixture: DifferentialFixture{
			Name: "dubbo-proxy", WireProtocol: differentialFixtureWireDubboProxyHessian2,
			ExpectedCalls: 1,
			SemanticHeaders: []string{
				differentialDubboProxyParamsTypeHeader,
				differentialDubboProxyHTTPHostHeader,
				differentialDubboProxyHTTPBodyHeader,
				"Extra-Arg-K",
			},
			Response: DifferentialFixtureResponse{
				Status:  http.StatusOK,
				Headers: map[string]string{"Got-extra-arg-k": differentialDubboProxyHeaderValue},
				Body:    differentialDubboProxyResponseBody,
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
