package pluginintegration

import (
	"encoding/json"
	"fmt"
	"time"
)

const differentialUDPLoggerFixtureMethod = "UDP"

func compareDifferentialUDPLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialRawLoggerFixtureDelivery(spec, left, right, policy, differentialRawLoggerFixtureContract{
		pinned:    mustDifferentialCase(spec.Name),
		rawMethod: differentialUDPLoggerFixtureMethod,
		validateBody: func(body string) (string, error) {
			if err := validateDifferentialUDPLoggerPayload(body, spec.RouteID); err != nil {
				return "", err
			}
			return "udp-datagram:validated-single-object-with-rfc3339-time", nil
		},
	})
}

func validateDifferentialUDPLoggerPayload(body, routeID string) error {
	fields, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{
			"@timestamp": {}, "case name": {}, "client_ip": {}, "host": {},
			"route_id": {},
		},
		[]string{"@timestamp", "case name", "client_ip", "host", "route_id"},
	)
	if err != nil {
		return err
	}
	if err := validateDifferentialRawLoggerStringFields(fields, map[string]string{
		"case name": "logger format in plugin",
		"client_ip": "127.0.0.1",
		"host":      "localhost",
		"route_id":  routeID,
	}); err != nil {
		return err
	}
	var timestamp string
	if err := json.Unmarshal(fields["@timestamp"], &timestamp); err != nil {
		return fmt.Errorf("@timestamp is not a string: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339, timestamp)
	if err != nil || parsed.IsZero() {
		return fmt.Errorf("@timestamp %q is not RFC3339: %v", timestamp, err)
	}
	return nil
}
