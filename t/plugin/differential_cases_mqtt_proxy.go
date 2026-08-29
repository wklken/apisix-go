package pluginintegration

const (
	differentialMQTTProxyCONNECTPolicy       = "mqtt-proxy-connect-forwarding"
	differentialFixtureWireMQTTCONNECT       = "mqtt-connect"
	differentialMQTTListenPortPlaceholder    = "__APISIX_GO_DIFFERENTIAL_MQTT_LISTEN_PORT__"
	differentialMQTTProxyInvalidPacket       = "mmm"
	differentialMQTTProxyFixtureResponseBody = "hello world"
)

type differentialMQTTSourceTest struct {
	File        string
	TestNumbers []int
}

type differentialMQTTProxyCase struct {
	Spec        DifferentialCase
	SourceTests []differentialMQTTSourceTest
}

// differentialMQTTProxyCases maps APISIX 3.17
// t/stream-plugin/mqtt-proxy.t TEST 2/3 to one real TCP sequence. The first
// connection must be rejected before it reaches the upstream fixture; the
// second forwards the exact pinned MQTT 3.1.1 CONNECT and receives the pinned
// fixture response. Route matching and chash node selection stay platform-owned.
func differentialMQTTProxyCases() []differentialMQTTProxyCase {
	const routeID = "differential-mqtt-proxy-connect"
	connect := string([]byte{
		0x10, 0x0f, 0x00, 0x04, 'M', 'Q', 'T', 'T',
		0x04, 0x02, 0x00, 0x3c, 0x00, 0x03, 'f', 'o', 'o',
	})
	step := func(body string) DifferentialStep {
		return DifferentialStep{
			Request:          DifferentialRequest{Method: "MQTT", Body: body},
			SecurityDecision: "not_applicable",
		}
	}
	return []differentialMQTTProxyCase{{
		Spec: DifferentialCase{
			Name:             "mqtt-proxy-rejects-invalid-then-forwards-connect",
			Plugin:           "mqtt-proxy",
			RouteID:          routeID,
			ComparisonPolicy: differentialMQTTProxyCONNECTPolicy,
			Config: map[string]any{
				"stream_routes": []any{map[string]any{
					"id":          routeID,
					"server_addr": "127.0.0.1",
					"server_port": differentialMQTTListenPortPlaceholder,
					"plugins": map[string]any{"mqtt-proxy": map[string]any{
						"protocol_name": "MQTT", "protocol_level": 4,
					}},
					"upstream": map[string]any{
						"type":   "roundrobin",
						"scheme": "tcp",
						"nodes":  map[string]any{differentialFixturePlaceholder: 1},
					},
				}},
			},
			Steps: []DifferentialStep{
				step(differentialMQTTProxyInvalidPacket),
				step(connect),
			},
			Fixture: DifferentialFixture{
				Name:          "mqtt-broker",
				WireProtocol:  differentialFixtureWireMQTTCONNECT,
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Body: differentialMQTTProxyFixtureResponseBody,
				},
			},
		},
		SourceTests: []differentialMQTTSourceTest{{
			File: "t/stream-plugin/mqtt-proxy.t", TestNumbers: []int{2, 3},
		}},
	}}
}

func differentialMQTTProxyCaseSpecs() []DifferentialCase {
	cases := differentialMQTTProxyCases()
	specs := make([]DifferentialCase, 0, len(cases))
	for _, mqttCase := range cases {
		specs = append(specs, mqttCase.Spec)
	}
	return specs
}

// differentialMQTTProxyRuntimeOverlay is deliberately kept outside the
// generic runtime overlay path: the generic differential harness owns HTTP
// listener settings and currently rejects apisix/stream_plugins overlays.
func differentialMQTTProxyRuntimeOverlay(listenAddress string) map[string]any {
	return map[string]any{
		"apisix": map[string]any{
			"proxy_mode": "http&stream",
			"stream_proxy": map[string]any{
				"tcp": []any{map[string]any{"addr": listenAddress}},
			},
		},
		"stream_plugins": []any{"mqtt-proxy"},
	}
}
