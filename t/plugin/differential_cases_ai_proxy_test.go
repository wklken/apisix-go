package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAIProxyCasesMatchPinnedAPISIX317EmptyRequestBody(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	const routeID = "differential-ai-proxy-empty-body"
	want := []DifferentialCase{{
		Name:    "ai-proxy-empty-request-body",
		Plugin:  "ai-proxy",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "differential-ai-proxy-prometheus-dependency",
					"uri": "/_differential/ai-proxy/prometheus",
					"plugins": map[string]any{
						"prometheus": map[string]any{},
					},
				},
				map[string]any{
					"id":  routeID,
					"uri": "/anything",
					"plugins": map[string]any{"ai-proxy": map[string]any{
						"provider": "openai",
						"auth": map[string]any{"header": map[string]any{
							"Authorization": "Bearer token",
						}},
						"options": map[string]any{
							"model":       "gpt-35-turbo-instruct",
							"max_tokens":  512,
							"temperature": 1.0,
						},
						"override": map[string]any{
							"endpoint": "http://" + differentialFixturePlaceholder,
						},
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

	got := differentialAIProxyCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialAIProxyCases() = %#v, want %#v", got, want)
	}
	if plugins := differentialRequiredPluginNames(
		got,
	); !reflect.DeepEqual(
		plugins,
		[]string{"ai-proxy", "prometheus"},
	) {
		t.Fatalf("required plugins = %v, want ai-proxy plus pinned prometheus dependency", plugins)
	}
}
