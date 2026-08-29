package pluginintegration

import "net/http"

// differentialOASValidatorCases maps APISIX 3.17 oas-validator.t TEST 3/4
// to one valid Pet request pass-through case. The inline document preserves
// the pinned Pet fields and its required name/photoUrls contract.
func differentialOASValidatorCases() []DifferentialCase {
	const (
		routeID = "differential-oas-validator-valid-body"
		openAPI = `{"openapi":"3.0.2","info":{"title":"Swagger Petstore - OpenAPI 3.0","version":"1.0.17"},"servers":[{"url":"/api/v3"}],"paths":{"/pet":{"post":{"requestBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Pet"}}}},"responses":{"200":{"description":"ok"}}}}},"components":{"schemas":{"Category":{"type":"object","properties":{"id":{"type":"integer","format":"int64"},"name":{"type":"string"}}},"Tag":{"type":"object","properties":{"id":{"type":"integer","format":"int64"},"name":{"type":"string"}}},"Pet":{"type":"object","required":["name","photoUrls"],"properties":{"id":{"type":"integer","format":"int64"},"name":{"type":"string"},"category":{"$ref":"#/components/schemas/Category"},"photoUrls":{"type":"array","items":{"type":"string"}},"tags":{"type":"array","items":{"$ref":"#/components/schemas/Tag"}},"status":{"type":"string","enum":["available","pending","sold"]}}}}}}`
	)

	return []DifferentialCase{
		{
			Name:    "oas-validator-valid-json-body",
			Plugin:  "oas-validator",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id": routeID, "uri": "/*",
					"plugins": map[string]any{
						"oas-validator": map[string]any{"spec": openAPI},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodPost,
				Path:   "/api/v3/pet",
				Host:   "gateway.example.test",
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
				Body: `{"id":10,"name":"doggie","category":{"id":1,"name":"Dogs"},"photoUrls":["string"],"tags":[{"id":1,"name":"tag1"}],"status":"available"}`,
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "oas accepted",
				},
			},
			SecurityDecision: "allow",
		},
	}
}
