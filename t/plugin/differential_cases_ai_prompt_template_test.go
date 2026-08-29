package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

// TEST 1 configures the named template and TEST 3 observes its rewrite.
func TestDifferentialAIPromptTemplateMatchesPinnedAPISIX317NamedRewrite(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "ai-prompt-template-named-model-rewrite",
		Plugin:  "ai-prompt-template",
		RouteID: "diff-ai-prompt-template-named",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-ai-prompt-template-named", "uri": "/template",
				"plugins": map[string]any{"ai-prompt-template": map[string]any{
					"templates": []any{map[string]any{
						"name": "programming question",
						"template": map[string]any{
							"model": "some model",
						},
					}},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/template", Host: "gateway.example.test",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"template_name":"programming question"}`,
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "templated"},
		},
		SecurityDecision: "not_applicable",
	}}

	got := differentialAIPromptTemplateCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialAIPromptTemplateCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
