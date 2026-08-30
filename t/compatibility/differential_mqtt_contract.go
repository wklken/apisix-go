package pluginintegration

const (
	differentialMQTTProxyCONNECTPolicy       = "mqtt-proxy-connect-forwarding"
	differentialFixtureWireMQTTCONNECT       = "mqtt-connect"
	differentialMQTTListenPortPlaceholder    = "__APISIX_GO_DIFFERENTIAL_MQTT_LISTEN_PORT__"
	differentialMQTTProxyInvalidPacket       = "mmm"
	differentialMQTTProxyFixtureResponseBody = "hello world"
)

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
