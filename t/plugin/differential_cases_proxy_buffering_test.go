package pluginintegration

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialProxyBufferingCasesCoverPinnedAPISIX317SSETransitBoundary(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialProxyBufferingCases()
	if len(cases) != 1 {
		t.Fatalf("differentialProxyBufferingCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "proxy-buffering-disabled-sse-transit" || spec.Plugin != "proxy-buffering" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-proxy-buffering-disabled" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", spec.ComparisonPolicy)
	}
	if spec.SecurityDecision != "not_applicable" {
		t.Fatalf("security decision = %q, want not_applicable", spec.SecurityDecision)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/events" ||
		spec.Request.Host != "gateway.example.test" ||
		spec.Request.Headers["Accept"] != "text/event-stream" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		len(spec.Fixture.SemanticHeaders) != 1 || spec.Fixture.SemanticHeaders[0] != "Accept" ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "text/event-stream" ||
		spec.Fixture.Response.Body != "data: event-1\n\ndata: event-2\n\ndata: event-3\n\n" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one route", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != spec.RouteID || route["uri"] != "/events" {
		t.Fatalf("route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	proxyBuffering, ok := plugins["proxy-buffering"].(map[string]any)
	if !ok || proxyBuffering["disable_proxy_buffering"] != true {
		t.Fatalf("proxy-buffering config = %#v, want disable_proxy_buffering true", plugins["proxy-buffering"])
	}
	config := string(mustYAML(t, spec.Config))
	if !strings.Contains(config, differentialFixturePlaceholder) {
		t.Fatalf("standalone config lacks fixture placeholder:\n%s", config)
	}

	raw, request, err := renderDifferentialOracleRequest(spec)
	if err != nil {
		t.Fatalf("render oracle request: %v", err)
	}
	if request.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("Accept = %q", request.Header.Get("Accept"))
	}
	if !bytes.Contains(raw, []byte("Accept: text/event-stream\r\n")) {
		t.Fatalf("raw request lacks SSE Accept header:\n%s", raw)
	}
}
