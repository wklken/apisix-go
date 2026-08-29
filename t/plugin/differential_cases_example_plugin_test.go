package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialExamplePluginCaseMatchesPinnedAPISIX317UpstreamOverride(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{
		{
			Name:    "example-plugin-overrides-route-upstream",
			Plugin:  "example-plugin",
			RouteID: "diff-example-plugin-upstream",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-example-plugin-upstream",
					"uri": "/server_port",
					"plugins": map[string]any{
						"example-plugin": map[string]any{
							"i":    11,
							"ip":   differentialFixtureHostPlaceholder,
							"port": differentialFixturePortPlaceholder,
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/server_port",
				Host:   "gateway.example.test",
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "example-plugin",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}

	got := differentialExamplePluginCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialExamplePluginCases() = %#v, want %#v", got, want)
	}
	for _, spec := range got {
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if spec.ComparisonPolicy != "" {
			t.Fatalf("case %q comparison policy = %q, want exact", spec.Name, spec.ComparisonPolicy)
		}
	}

	projected, err := projectDifferentialConfig(got[0].Config, "127.0.0.1:23456")
	if err != nil {
		t.Fatalf("projectDifferentialConfig() error = %v", err)
	}
	route := projected["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["example-plugin"].(map[string]any)
	if gotIP, ok := pluginConfig["ip"].(string); !ok || gotIP != "127.0.0.1" {
		t.Fatalf("projected example-plugin ip = %#v, want string 127.0.0.1", pluginConfig["ip"])
	}
	if gotPort, ok := pluginConfig["port"].(int); !ok || gotPort != 23456 {
		t.Fatalf("projected example-plugin port = %#v, want int 23456", pluginConfig["port"])
	}
}
