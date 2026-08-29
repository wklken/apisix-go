package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialWorkflowCasesCoverPinnedAPISIX317FirstMatchingRule(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialWorkflowCases()
	if len(cases) != 1 {
		t.Fatalf("differentialWorkflowCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "workflow-first-matching-rule-stops" || spec.Plugin != "workflow" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello?foo=bar" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("fixture = %#v, want primary not called", spec.Fixture)
	}
	if spec.SecurityDecision != "deny" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want deny/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["workflow"].(map[string]any)
	rules := pluginConfig["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("workflow rules = %#v, want two ordered rules", rules)
	}
	wantFirstCase := []any{[]any{"arg_foo", "==", "bar"}}
	wantSecondCase := []any{[]any{"uri", "==", "/hello"}}
	if !reflect.DeepEqual(rules[0].(map[string]any)["case"], wantFirstCase) ||
		!reflect.DeepEqual(rules[1].(map[string]any)["case"], wantSecondCase) {
		t.Fatalf("workflow rule order = %#v", rules)
	}
	if !reflect.DeepEqual(rules[0].(map[string]any)["actions"], []any{[]any{"return", map[string]any{"code": 403}}}) ||
		!reflect.DeepEqual(rules[1].(map[string]any)["actions"], []any{[]any{"return", map[string]any{"code": 401}}}) {
		t.Fatalf("workflow actions = %#v", rules)
	}
}
