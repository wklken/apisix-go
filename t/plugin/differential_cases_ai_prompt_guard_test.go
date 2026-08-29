package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAIPromptGuardMatchesPinnedAPISIX317AnchoredDeny(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "ai-prompt-guard-anchored-deny",
		Plugin:  "ai-prompt-guard",
		RouteID: "diff-ai-prompt-guard-deny",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-ai-prompt-guard-deny", "uri": "/v1/chat/completions",
				"plugins": map[string]any{"ai-prompt-guard": map[string]any{
					"match_all_roles": true,
					"deny_patterns":   []any{"^badword$"},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/v1/chat/completions", Host: "gateway.example.test",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"messages":[{"role":"system","content":"badword"}]}`,
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		SecurityDecision: "deny",
	}}

	got := differentialAIPromptGuardCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialAIPromptGuardCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
	if got[0].RouteID == "" || len(got[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want 1..64", len(got[0].RouteID))
	}
}
