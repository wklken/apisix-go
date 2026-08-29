package pluginintegration

import (
	"encoding/json"
	"fmt"
	"reflect"
)

func init() {
	differentialComparatorRegistry[differentialRocketMQLoggerPublishPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"rocketmq-logger": {}},
		compare:        compareDifferentialRocketMQLoggerPublish,
	}
}

func compareDifferentialRocketMQLoggerPublish(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialRocketMQLoggerCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned rocketmq-logger case",
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
		if err := normalizeDifferentialRocketMQLoggerObservation(
			spec, side.name, side.observation,
		); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialRocketMQLoggerObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if len(observation.Steps) != 0 || observation.Status != spec.Fixture.Response.Status ||
		observation.Body != spec.Fixture.Response.Body || observation.Host != spec.Request.Host ||
		observation.SNI != spec.Request.SNI || observation.SecurityDecision != spec.SecurityDecision {
		return fmt.Errorf("comparison policy %q requires the exact %s gateway response", spec.ComparisonPolicy, side)
	}
	if err := normalizeDifferentialNetworkLoggerGatewayHeaders(
		side,
		observation.Headers,
		len(spec.Fixture.Response.Body),
		"text/plain; charset=utf-8",
		"text/plain; charset=utf-8",
	); err != nil {
		return fmt.Errorf("comparison policy %q %s gateway headers: %w", spec.ComparisonPolicy, side, err)
	}
	if observation.RetryCount != 0 || len(observation.RouteObserver) != 0 ||
		observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" ||
		len(observation.UpstreamCalls) != spec.Fixture.ExpectedCalls ||
		!differentialLoggerUpstreamIsCapturedCall(observation.Upstream, observation.UpstreamCalls) {
		return fmt.Errorf(
			"comparison policy %q requires exactly one %s origin call and one RocketMQ message",
			spec.ComparisonPolicy,
			side,
		)
	}

	origin := observation.UpstreamCalls[0]
	if !origin.Received || origin.Fixture != spec.Fixture.Name || origin.Method != spec.Request.Method ||
		origin.Path != spec.Request.Path || origin.Host != "differential.example.test" ||
		len(origin.Headers) != 0 || origin.Body != spec.Request.Body {
		return fmt.Errorf("comparison policy %q %s origin request is not exact", spec.ComparisonPolicy, side)
	}
	message := observation.UpstreamCalls[1]
	if !message.Received || message.Fixture != spec.Fixture.Name ||
		message.Method != differentialRocketMQMethod || message.Path != "test2" || message.Host != "key1" ||
		!reflect.DeepEqual(message.Headers, map[string][]string{
			differentialRocketMQTagHeader:     {"tag1"},
			differentialRocketMQQueueIDHeader: {"0"},
		}) {
		return fmt.Errorf(
			"comparison policy %q %s RocketMQ topic/key/tag/queue envelope is not exact",
			spec.ComparisonPolicy,
			side,
		)
	}
	canonical, err := canonicalDifferentialRocketMQRouteFormatEntry(message.Body, spec.RouteID)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s RocketMQ message: %w", spec.ComparisonPolicy, side, err)
	}
	message.Body = canonical
	observation.UpstreamCalls = []DifferentialUpstreamObservation{origin, message}
	observation.Upstream = message
	return nil
}

func canonicalDifferentialRocketMQRouteFormatEntry(raw string, routeID string) (string, error) {
	var entry map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return "", fmt.Errorf("decode route-format JSON object: %w", err)
	}
	if len(entry) != 2 {
		return "", fmt.Errorf("route-format JSON field count = %d, want 2", len(entry))
	}
	var actualRouteID string
	if err := json.Unmarshal(entry["route_id"], &actualRouteID); err != nil {
		return "", fmt.Errorf("decode route_id: %w", err)
	}
	if actualRouteID != routeID {
		return "", fmt.Errorf("route_id = %q, want %q", actualRouteID, routeID)
	}
	var clientIP string
	if err := json.Unmarshal(entry["x_ip"], &clientIP); err != nil {
		return "", fmt.Errorf("decode x_ip: %w", err)
	}
	if clientIP != "127.0.0.1" {
		return "", fmt.Errorf("x_ip = %q, want 127.0.0.1", clientIP)
	}
	return fmt.Sprintf(`{"route_id":%q,"x_ip":"127.0.0.1"}`, routeID), nil
}
