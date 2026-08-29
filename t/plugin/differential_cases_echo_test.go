package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialEchoCasesMatchAPISIX317ReplaceBodyAndHeaders(t *testing.T) {
	cases := differentialEchoCases()
	if len(cases) != 1 {
		t.Fatalf("differentialEchoCases() = %d cases, want 1", len(cases))
	}

	want := DifferentialCase{
		Name:    "echo-replace-body-and-add-headers",
		Plugin:  "echo",
		RouteID: "differential-echo-replace-body-and-add-headers",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  "differential-echo-replace-body-and-add-headers",
				"uri": "/hello",
				"plugins": map[string]any{
					"echo": map[string]any{
						"before_body": "before the body modification ",
						"body":        "hello upstream",
						"after_body":  " after the body modification.",
						"headers": map[string]any{
							"Location":      "https://www.iresty.com",
							"Authorization": "userpass",
						},
					},
				},
				"upstream": map[string]any{
					"nodes": map[string]any{differentialFixturePlaceholder: 1},
					"type":  "roundrobin",
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/hello",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "hello world",
			},
		},
		SecurityDecision: "not_applicable",
	}
	if !reflect.DeepEqual(cases[0], want) {
		t.Fatalf("differentialEchoCases()[0] = %#v, want %#v", cases[0], want)
	}
}
