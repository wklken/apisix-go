package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialTrafficLabelCaseMatchesPinnedAPISIX317HeaderOverride(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{
		{
			Name:    "traffic-label-overrides-request-header",
			Plugin:  "traffic-label",
			RouteID: "diff-traffic-label-header-override",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-traffic-label-header-override",
					"uri": "/echo",
					"plugins": map[string]any{
						"traffic-label": map[string]any{
							"rules": []any{map[string]any{
								"match": []any{[]any{"arg_foo", "==", "bar"}},
								"actions": []any{map[string]any{
									"set_headers": map[string]any{"X-Route-Observer": 100},
								}},
							}},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/echo?foo=bar",
				Host:    "gateway.example.test",
				Headers: map[string]string{"X-Route-Observer": "200"},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "traffic-label",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}

	got := differentialTrafficLabelCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialTrafficLabelCases() = %#v, want %#v", got, want)
	}
	for _, spec := range got {
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if spec.ComparisonPolicy != "" {
			t.Fatalf("case %q comparison policy = %q, want exact", spec.Name, spec.ComparisonPolicy)
		}
		if original := spec.Request.Headers["X-Route-Observer"]; original != "200" {
			t.Fatalf("case %q original X-Route-Observer = %q, want 200", spec.Name, original)
		}
		route := spec.Config["routes"].([]any)[0].(map[string]any)
		pluginConfig := route["plugins"].(map[string]any)["traffic-label"].(map[string]any)
		rule := pluginConfig["rules"].([]any)[0].(map[string]any)
		action := rule["actions"].([]any)[0].(map[string]any)
		setHeaders := action["set_headers"].(map[string]any)
		if overwritten := setHeaders["X-Route-Observer"]; overwritten != 100 {
			t.Fatalf("case %q overwritten X-Route-Observer = %#v, want 100", spec.Name, overwritten)
		}
	}
}
