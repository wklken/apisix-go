package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialGoogleCloudLoggingCasesCapturePinnedOAuthAndEntryDelivery(t *testing.T) {
	cases := differentialGoogleCloudLoggingCases()
	if len(cases) != 1 {
		t.Fatalf("google-cloud-logging cases = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "google-cloud-logging-exchanges-jwt-and-writes-custom-entry" ||
		spec.Plugin != "google-cloud-logging" ||
		spec.RouteID != differentialGoogleCloudLoggingRouteID ||
		spec.ComparisonPolicy != differentialGoogleCloudLoggingFixtureDeliveryPolicy {
		t.Fatalf("case identity = %#v", spec)
	}
	if len(spec.Steps) != 1 || spec.Steps[0].Request.Method != http.MethodGet ||
		spec.Steps[0].Request.Path != differentialGoogleCloudLoggingGatewayPath ||
		spec.Steps[0].Request.Host != "gateway.example.test" ||
		spec.Steps[0].SecurityDecision != "not_applicable" {
		t.Fatalf("gateway step = %#v", spec.Steps)
	}
	if spec.Fixture.Name != "origin-and-google-cloud-logging" ||
		spec.Fixture.ExpectedCalls != 3 || !spec.Fixture.CaptureAllCalls ||
		spec.Fixture.CollectTimeoutMillis != 7000 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "application/json" ||
		spec.Fixture.Response.Body != differentialGoogleCloudLoggingTokenResponse {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if strings.Join(spec.Fixture.SemanticHeaders, ",") != "Authorization,Content-Type" {
		t.Fatalf("semantic headers = %v", spec.Fixture.SemanticHeaders)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", spec.Config["routes"])
	}
	route := routes[0].(map[string]any)
	plugins := route["plugins"].(map[string]any)
	config := plugins["google-cloud-logging"].(map[string]any)
	auth := config["auth_config"].(map[string]any)
	if auth["client_email"] != differentialGoogleCloudLoggingClientEmail ||
		auth["project_id"] != differentialGoogleCloudLoggingProjectID ||
		auth["token_uri"] != "http://"+differentialFixturePlaceholder+differentialGoogleCloudLoggingTokenPath ||
		auth["entries_uri"] != "http://"+differentialFixturePlaceholder+differentialGoogleCloudLoggingEntriesPath ||
		!strings.Contains(auth["private_key"].(string), "BEGIN PRIVATE KEY") {
		t.Fatalf("auth config = %#v", auth)
	}
	scopes := auth["scope"].([]any)
	if len(scopes) != 1 || scopes[0] != differentialGoogleCloudLoggingScope {
		t.Fatalf("scope = %#v", scopes)
	}
	resource := config["resource"].(map[string]any)
	labels := resource["labels"].(map[string]any)
	if resource["type"] != "global" || labels["project_id"] != differentialGoogleCloudLoggingProjectID ||
		config["log_id"] != differentialGoogleCloudLoggingLogID {
		t.Fatalf("entry config = %#v", config)
	}
	logFormat := config["log_format"].(map[string]any)
	if logFormat["case"] != "google-cloud-logging" || logFormat["route_id"] != "$route_id" ||
		config["batch_max_size"] != 1 || config["inactive_timeout"] != 1 ||
		config["max_retry_count"] != 0 {
		t.Fatalf("logger config = %#v", config)
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream = %#v", upstream)
	}
}
