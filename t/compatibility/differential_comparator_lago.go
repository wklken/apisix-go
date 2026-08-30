package pluginintegration

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
)

func compareDifferentialLagoFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:       mustDifferentialCase(spec.Name),
		loggerMethod: http.MethodPost,
		loggerPath:   "/api/v1/events/batch",
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
				"Authorization": "Bearer differential-token",
				"Content-Type":  "application/json",
			}); err != nil {
				return fmt.Errorf("%s Lago headers: %w", side, err)
			}
			if err := validateDifferentialLagoPayload(call.Body, spec.RouteID); err != nil {
				return fmt.Errorf("%s Lago payload: %w", side, err)
			}
			call.Body = "lago-payload:validated-single-event"
			return nil
		},
	})
}

func validateDifferentialLagoPayload(body string, routeID string) error {
	root, err := decodeDifferentialJSONObject(
		body, map[string]struct{}{"events": {}}, []string{"events"},
	)
	if err != nil {
		return err
	}
	events, err := decodeDifferentialJSONArray(root["events"])
	if err != nil || len(events) != 1 {
		return fmt.Errorf("events must contain exactly one object: %v", err)
	}
	event, err := decodeDifferentialJSONObject(
		string(events[0]),
		map[string]struct{}{
			"transaction_id": {}, "external_subscription_id": {}, "code": {},
			"timestamp": {}, "properties": {},
		},
		[]string{"transaction_id", "external_subscription_id", "code", "timestamp", "properties"},
	)
	if err != nil {
		return err
	}
	for field, want := range map[string]string{
		"transaction_id": "differential-request", "external_subscription_id": "differential-subscription",
		"code": "differential-usage",
	} {
		var got string
		if err := json.Unmarshal(event[field], &got); err != nil || got != want {
			return fmt.Errorf("%s = %q, want %q: %v", field, got, want, err)
		}
	}
	var timestamp json.Number
	if err := json.Unmarshal(event["timestamp"], &timestamp); err != nil {
		return fmt.Errorf("timestamp is not a number: %w", err)
	}
	parsedTimestamp, err := timestamp.Float64()
	if err != nil || parsedTimestamp <= 0 || math.IsInf(parsedTimestamp, 0) || math.IsNaN(parsedTimestamp) {
		return fmt.Errorf("timestamp = %q, want positive finite Unix time: %v", timestamp, err)
	}
	properties, err := decodeDifferentialJSONObject(
		string(event["properties"]),
		map[string]struct{}{"route": {}, "status": {}},
		[]string{"route", "status"},
	)
	if err != nil {
		return err
	}
	for field, want := range map[string]string{"route": routeID, "status": "200"} {
		var got string
		if err := json.Unmarshal(properties[field], &got); err != nil || got != want {
			return fmt.Errorf("property %s = %q, want %q: %v", field, got, want, err)
		}
	}
	return nil
}
