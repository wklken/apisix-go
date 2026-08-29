package pluginintegration

import "net/http"

const (
	differentialMCPBridgeSSESessionPolicy = "mcp-bridge-sse-session"
	differentialMCPBridgeEchoCommand      = `while IFS= read -r line; do printf '%s\n' "$line"; done`
	differentialMCPBridgePostedPayload    = `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`
)

// differentialMCPBridgeCases is additional runtime evidence against the
// implementation pinned by compatibilityOracleSourceCommit. APISIX 3.17's
// sole t/plugin/mcp-bridge.t label is a schema matrix; this case deliberately
// does not claim that TEST 1 is a runtime mapping.
func differentialMCPBridgeCases() []DifferentialCase {
	const routeID = "differential-mcp-bridge-session"
	return []DifferentialCase{{
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
}
