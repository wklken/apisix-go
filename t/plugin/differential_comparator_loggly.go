package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func compareDifferentialLogglyHTTPFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:       differentialLogglyCases()[0],
		loggerMethod: http.MethodPost,
		loggerPath:   differentialLogglyBulkPath,
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
				"Content-Type": "application/json",
				"X-LOGGLY-TAG": "apisix",
			}); err != nil {
				return fmt.Errorf("%s Loggly headers: %w", side, err)
			}
			if err := validateDifferentialLogglyPayload(call.Body, spec.RouteID); err != nil {
				return fmt.Errorf("%s Loggly payload: %w", side, err)
			}
			call.Body = `{"case":"loggly","route_id":"` + spec.RouteID +
				`","timestamp":"<rfc3339>"}`
			return nil
		},
	})
}

func validateDifferentialLogglyPayload(body string, routeID string) error {
	fields, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{"case": {}, "route_id": {}, "timestamp": {}},
		[]string{"case", "route_id", "timestamp"},
	)
	if err != nil {
		return err
	}

	var customCase string
	if err := json.Unmarshal(fields["case"], &customCase); err != nil {
		return fmt.Errorf("field case is not a string: %w", err)
	}
	if customCase != "loggly" {
		return fmt.Errorf("field %q = %q, want %q", "case", customCase, "loggly")
	}

	var gotRouteID string
	if err := json.Unmarshal(fields["route_id"], &gotRouteID); err != nil {
		return fmt.Errorf("field route_id is not a string: %w", err)
	}
	if gotRouteID != routeID {
		return fmt.Errorf("field route_id = %q, want %q", gotRouteID, routeID)
	}

	var timestamp string
	if err := json.Unmarshal(fields["timestamp"], &timestamp); err != nil {
		return fmt.Errorf("field timestamp is not a string: %w", err)
	}
	if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
		return fmt.Errorf("field timestamp = %q is not RFC3339: %w", timestamp, err)
	}
	return nil
}
