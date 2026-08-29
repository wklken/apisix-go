package pluginintegration

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestDifferentialProxyControlCasesCoverPinnedAPISIX317BufferingOff(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialProxyControlCases()
	if len(cases) != 1 {
		t.Fatalf("differentialProxyControlCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "proxy-control-request-buffering-off-large-body" || spec.Plugin != "proxy-control" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-proxy-control-buffering-off" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", spec.ComparisonPolicy)
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/hello" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if len(spec.Request.Body) != 5*10240 || strings.Count(spec.Request.Body, "12345") != 10240 {
		t.Fatalf(
			"request body length/pattern = %d/%d, want 51200/10240",
			len(spec.Request.Body),
			strings.Count(spec.Request.Body, "12345"),
		)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "hello world" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "not_applicable" {
		t.Fatalf("security decision = %q, want not_applicable", spec.SecurityDecision)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one route", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != spec.RouteID || route["uri"] != "/hello" {
		t.Fatalf("route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	proxyControl, ok := plugins["proxy-control"].(map[string]any)
	if !ok || proxyControl["request_buffering"] != false {
		t.Fatalf("proxy-control config = %#v, want request_buffering false", plugins["proxy-control"])
	}
	config := string(mustYAML(t, spec.Config))
	if !strings.Contains(config, differentialFixturePlaceholder) {
		t.Fatalf("standalone config lacks fixture placeholder:\n%s", config)
	}

	raw, request, err := renderDifferentialOracleRequest(spec)
	if err != nil {
		t.Fatalf("render oracle request: %v", err)
	}
	if request.ContentLength != 51200 {
		t.Fatalf("ContentLength = %d, want 51200", request.ContentLength)
	}
	if !bytes.Contains(raw, []byte("Content-Length: "+strconv.Itoa(51200)+"\r\n")) {
		t.Fatalf("raw request lacks Content-Length 51200")
	}
	if !bytes.HasSuffix(raw, []byte(spec.Request.Body)) {
		t.Fatal("raw request does not end with the exact large body")
	}
}
