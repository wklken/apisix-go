package pluginintegration

import (
	"net/http"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestDifferentialDegraphqlCasesHaveRunnableShape(t *testing.T) {
	cases := differentialDegraphqlCases()
	if len(cases) != 2 {
		t.Fatalf("differentialDegraphqlCases() = %d cases, want 2", len(cases))
	}

	want := []struct {
		name   string
		method string
		path   string
	}{
		{name: "degraphql-post-query-without-variables", method: http.MethodPost, path: "/graphql"},
		{name: "degraphql-get-query-without-variables", method: http.MethodGet, path: "/graphql"},
	}
	seenRouteIDs := make(map[string]struct{}, len(cases))
	for i, spec := range cases {
		if spec.Name != want[i].name {
			t.Errorf("case %d name = %q, want %q", i, spec.Name, want[i].name)
		}
		if spec.Plugin != "degraphql" {
			t.Errorf("case %q plugin = %q, want degraphql", spec.Name, spec.Plugin)
		}
		if spec.Request.Method != want[i].method || spec.Request.Path != want[i].path {
			t.Errorf(
				"case %q request = %s %s, want %s %s",
				spec.Name,
				spec.Request.Method,
				spec.Request.Path,
				want[i].method,
				want[i].path,
			)
		}
		if spec.Request.Host != "gateway.example.test" {
			t.Errorf("case %q host = %q, want gateway.example.test", spec.Name, spec.Request.Host)
		}
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Errorf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if _, duplicate := seenRouteIDs[spec.RouteID]; duplicate {
			t.Errorf("duplicate route ID %q", spec.RouteID)
		}
		seenRouteIDs[spec.RouteID] = struct{}{}
		if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 {
			t.Errorf("case %q fixture = %#v, want primary called once", spec.Name, spec.Fixture)
		}
		if spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "graphql fixture response" {
			t.Errorf("case %q response = %#v, want deterministic 200 fixture", spec.Name, spec.Fixture.Response)
		}
		if spec.SecurityDecision != "not_applicable" {
			t.Errorf("case %q security decision = %q, want not_applicable", spec.Name, spec.SecurityDecision)
		}
	}
}

func TestDifferentialDegraphqlCasesMatchAPISIX317SourceBehavior(t *testing.T) {
	const query = "{persons{id}}"

	for _, spec := range differentialDegraphqlCases() {
		encoded, err := yaml.Marshal(spec.Config)
		if err != nil {
			t.Fatalf("marshal case %q config: %v", spec.Name, err)
		}
		config := string(encoded)
		for _, required := range []string{
			"degraphql:",
			"query: '{persons{id}}'",
			differentialFixturePlaceholder,
		} {
			if !strings.Contains(config, required) {
				t.Errorf("case %q config does not contain %q:\n%s", spec.Name, required, config)
			}
		}
		if strings.Contains(config, "variables:") || strings.Contains(config, "operation_name:") {
			t.Errorf("case %q unexpectedly configures variables or operation_name:\n%s", spec.Name, config)
		}
		if spec.Request.Body != "" {
			t.Errorf("case %q input body = %q, want empty source request", spec.Name, spec.Request.Body)
		}

		routes := spec.Config["routes"].([]any)
		route := routes[0].(map[string]any)
		pluginConfig := route["plugins"].(map[string]any)["degraphql"].(map[string]any)
		if got := pluginConfig["query"]; got != query {
			t.Errorf("case %q query = %#v, want %q", spec.Name, got, query)
		}
	}
}
