package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialHTTPDubboCaseMapsPinnedAPISIX317SerializedPOJO(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	cases := differentialHTTPDubboCases()
	if len(cases) != 1 {
		t.Fatalf("differentialHTTPDubboCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "http-dubbo-pojo-fastjson" || spec.Plugin != "http-dubbo" ||
		spec.RouteID != "differential-http-dubbo-pojo-fastjson" {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.RouteID)
	}
	if spec.ComparisonPolicy != differentialHTTPDubboPOJOPolicy ||
		spec.SecurityDecision != "not_applicable" {
		t.Fatalf("policy/decision = %q/%q", spec.ComparisonPolicy, spec.SecurityDecision)
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/t" ||
		spec.Request.Host != "gateway.example.test" || spec.Request.Body != differentialHTTPDubboPOJOJSON {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "http-dubbo" ||
		spec.Fixture.WireProtocol != differentialFixtureWireHTTPDubboFastJSON ||
		spec.Fixture.ExpectedCalls != 1 || spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Body != differentialHTTPDubboPOJOJSON {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if len(spec.Fixture.SemanticHeaders) != 1 ||
		spec.Fixture.SemanticHeaders[0] != differentialHTTPDubboParamsTypeHeader {
		t.Fatalf("fixture semantic headers = %#v", spec.Fixture.SemanticHeaders)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", spec.Config["routes"])
	}
	route := routes[0].(map[string]any)
	if route["id"] != spec.RouteID || route["uri"] != "/t" {
		t.Fatalf("route identity = %#v", route)
	}
	plugins := route["plugins"].(map[string]any)
	config := plugins["http-dubbo"].(map[string]any)
	if config["service_name"] != differentialHTTPDubboServiceName ||
		config["service_version"] != differentialHTTPDubboServiceVersion ||
		config["method"] != differentialHTTPDubboMethodName ||
		config["params_type_desc"] != differentialHTTPDubboParamsTypeDesc ||
		config["serialized"] != true {
		t.Fatalf("plugin config = %#v", config)
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["type"] != "roundrobin" ||
		upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream = %#v", upstream)
	}
}
