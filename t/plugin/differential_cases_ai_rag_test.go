package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAIRAGCasesCoverPinnedAPISIX317EmptyBody(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "ai-rag-empty-request-body",
		Plugin:  "ai-rag",
		RouteID: "differential-ai-rag-empty-body",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  "differential-ai-rag-empty-body",
				"uri": "/echo",
				"plugins": map[string]any{
					"ai-rag": map[string]any{
						"embeddings_provider": map[string]any{
							"azure_openai": map[string]any{
								"endpoint": "http://" + differentialFixturePlaceholder + "/embeddings",
								"api_key":  "key",
							},
						},
						"vector_search_provider": map[string]any{
							"azure_ai_search": map[string]any{
								"endpoint": "http://" + differentialFixturePlaceholder + "/search",
								"api_key":  "wrongkey",
							},
						},
					},
				},
				"upstream": map[string]any{
					"type":      "roundrobin",
					"nodes":     map[string]any{differentialFixturePlaceholder: 1},
					"scheme":    "http",
					"pass_host": "node",
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/echo",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected",
			},
		},
		SecurityDecision: "deny",
	}}

	got := differentialAIRAGCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialAIRAGCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
}
