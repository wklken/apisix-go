package pluginintegration

import (
	"bytes"
	"fmt"
	"reflect"
)

func init() {
	differentialComparatorRegistry[differentialMQTTProxyCONNECTPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"mqtt-proxy": {}},
		compare:        compareDifferentialMQTTProxyCONNECT,
	}
}

func compareDifferentialMQTTProxyCONNECT(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialMQTTProxyCases()[0].Spec) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned mqtt-proxy case",
			spec.ComparisonPolicy,
		)
	}
	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		if err := normalizeDifferentialMQTTProxyObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialMQTTProxyObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != 0 || len(observation.Headers) != 0 || observation.Body != "" ||
		observation.Host != "" || observation.SNI != "" || observation.SecurityDecision != "" ||
		observation.RetryCount != 0 || len(observation.RouteObserver) != 0 || observation.File != nil {
		return fmt.Errorf("%s mqtt-proxy raw TCP envelope is not exact", side)
	}
	if len(observation.Steps) != 2 {
		return fmt.Errorf("%s mqtt-proxy step count = %d, want 2", side, len(observation.Steps))
	}
	invalid := &observation.Steps[0]
	if invalid.Status != 0 || len(invalid.Headers) != 0 || invalid.Body != "" ||
		invalid.Host != "" || invalid.SNI != "" || invalid.SecurityDecision != "not_applicable" {
		return fmt.Errorf("%s mqtt-proxy invalid-header connection was not rejected without a response", side)
	}
	forwarded := &observation.Steps[1]
	if forwarded.Status != 0 || len(forwarded.Headers) != 0 ||
		forwarded.Body != spec.Fixture.Response.Body || forwarded.Host != "" ||
		forwarded.SNI != "" || forwarded.SecurityDecision != "not_applicable" {
		return fmt.Errorf("%s mqtt-proxy forwarded CONNECT response is not exact", side)
	}
	if observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" {
		return fmt.Errorf("%s mqtt-proxy upstream selection is not exact", side)
	}
	if len(observation.UpstreamCalls) != 1 {
		return fmt.Errorf("%s mqtt-proxy must expose exactly one upstream CONNECT", side)
	}
	upstream := &observation.Upstream
	if !reflect.DeepEqual(*upstream, observation.UpstreamCalls[0]) {
		return fmt.Errorf("%s mqtt-proxy upstream CONNECT summary is inconsistent", side)
	}
	if !upstream.Received || upstream.Fixture != spec.Fixture.Name ||
		upstream.Method != "MQTT" || upstream.Path != "CONNECT" ||
		upstream.Host != "" || len(upstream.Headers) != 0 {
		return fmt.Errorf("%s mqtt-proxy must expose exactly one upstream CONNECT", side)
	}
	wantPacket := []byte(spec.Steps[1].Request.Body)
	gotPacket := []byte(upstream.Body)
	if !bytes.Equal(gotPacket, wantPacket) {
		return fmt.Errorf("%s mqtt-proxy upstream did not receive the exact pinned CONNECT", side)
	}
	info, err := parseDifferentialMQTTCONNECT(gotPacket)
	if err != nil {
		return fmt.Errorf("%s mqtt-proxy upstream CONNECT: %w", side, err)
	}
	if info.ProtocolName != "MQTT" || info.ProtocolLevel != 4 ||
		info.ConnectFlags != 0x02 || info.KeepAlive != 60 || info.ClientID != "foo" {
		return fmt.Errorf("%s mqtt-proxy CONNECT semantics = %#v", side, info)
	}

	observation.Headers = nil
	invalid.Headers = nil
	forwarded.Headers = nil
	upstream.Headers = nil
	observation.UpstreamCalls[0].Headers = nil
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	return nil
}
