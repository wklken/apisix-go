package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAIProxyMultiCasesMatchPinnedAPISIX317EmptyRequestBody(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	const routeID = "differential-ai-proxy-multi-empty-body"
	want := []DifferentialCase{{
		Name:    "ai-proxy-multi-empty-request-body",
		Plugin:  "ai-proxy-multi",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "differential-ai-proxy-multi-prometheus-dependency",
					"uri": "/_differential/ai-proxy-multi/prometheus",
					"plugins": map[string]any{
						"prometheus": map[string]any{},
					},
				},
				map[string]any{
					"id":  routeID,
					"uri": "/anything",
					"plugins": map[string]any{"ai-proxy-multi": map[string]any{
						"instances": []any{map[string]any{
							"name":     "openai-official",
							"provider": "openai",
							"weight":   1,
							"auth": map[string]any{"header": map[string]any{
								"Authorization": "Bearer token",
							}},
							"options": map[string]any{
								"model":       "gpt-4",
								"max_tokens":  512,
								"temperature": 1.0,
							},
							"override": map[string]any{
								"endpoint": "http://" + differentialFixturePlaceholder,
							},
						}},
						"ssl_verify": false,
					}},
				},
			},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/anything",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"Authorization": "Bearer token",
			},
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected provider call",
			},
		},
		SecurityDecision: "deny",
	}}

	got := differentialAIProxyMultiCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialAIProxyMultiCases() = %#v, want %#v", got, want)
	}
	if plugins := differentialRequiredPluginNames(
		got,
	); !reflect.DeepEqual(
		plugins,
		[]string{"ai-proxy-multi", "prometheus"},
	) {
		t.Fatalf("required plugins = %v, want ai-proxy-multi plus pinned prometheus dependency", plugins)
	}
}
