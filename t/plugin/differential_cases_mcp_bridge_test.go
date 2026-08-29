package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialMCPBridgeCasesPinsPersistentSSESessionFlow(t *testing.T) {
	const routeID = "differential-mcp-bridge-session"
	want := []DifferentialCase{{
		Name:             "mcp-bridge-forwards-message-over-dynamic-sse-session",
		Plugin:           "mcp-bridge",
		RouteID:          routeID,
		ComparisonPolicy: differentialMCPBridgeSSESessionPolicy,
		Config: map[string]any{"routes": []any{map[string]any{
			"id": routeID, "uri": "/mcp/*",
			"plugins": map[string]any{"mcp-bridge": map[string]any{
				"base_uri": "/mcp", "command": "sh",
				"args": []any{"-c", differentialMCPBridgeEchoCommand},
			}},
			"upstream": differentialUpstream(),
		}}},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/mcp/sse", Host: "gateway.example.test",
			Headers: map[string]string{"Accept": "text/event-stream"},
		},
		Fixture: DifferentialFixture{
			Name: "unused", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		SecurityDecision: "not_applicable",
	}}

	if got := differentialMCPBridgeCases(); !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialMCPBridgeCases() = %#v, want %#v", got, want)
	}
}
