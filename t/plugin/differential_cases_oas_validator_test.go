package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialOASValidatorCasesCoverPinnedAPISIX317ValidRequest(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialOASValidatorCases()
	if len(cases) != 1 {
		t.Fatalf("differentialOASValidatorCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "oas-validator-valid-json-body" || spec.Plugin != "oas-validator" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/api/v3/pet" ||
		spec.Request.Headers["Content-Type"] != "application/json" ||
		spec.Request.Body != `{"id":10,"name":"doggie","category":{"id":1,"name":"Dogs"},"photoUrls":["string"],"tags":[{"id":1,"name":"tag1"}],"status":"available"}` {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "oas accepted" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "allow" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want allow/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["oas-validator"].(map[string]any)
	openAPI, ok := pluginConfig["spec"].(string)
	if !ok || !strings.Contains(openAPI, `"openapi":"3.0.2"`) ||
		!strings.Contains(openAPI, `"info":{"title":"Swagger Petstore - OpenAPI 3.0","version":"1.0.17"}`) ||
		!strings.Contains(openAPI, `"required":["name","photoUrls"]`) ||
		!strings.Contains(openAPI, `"photoUrls":{"type":"array","items":{"type":"string"}}`) ||
		!strings.Contains(openAPI, `"status":{"type":"string","enum":["available","pending","sold"]}`) {
		t.Fatalf("oas-validator spec = %#v", pluginConfig["spec"])
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream = %#v", upstream)
	}
}
