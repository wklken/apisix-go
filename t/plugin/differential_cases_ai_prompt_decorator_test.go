package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAIPromptDecoratorMatchesPinnedAPISIX317ResponsesAppend(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "ai-prompt-decorator-append-responses-input",
		Plugin:  "ai-prompt-decorator",
		RouteID: "diff-ai-prompt-decorator-append",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-ai-prompt-decorator-append", "uri": "/v1/responses",
				"plugins": map[string]any{"ai-prompt-decorator": map[string]any{
					"append": []any{map[string]any{
						"role": "user", "content": "Please be concise",
					}},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/v1/responses", Host: "gateway.example.test",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"input":"What is 1+1?"}`,
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "decorated"},
		},
		SecurityDecision: "not_applicable",
	}}

	got := differentialAIPromptDecoratorCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialAIPromptDecoratorCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
