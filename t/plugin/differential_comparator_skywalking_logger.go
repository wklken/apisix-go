package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func compareDifferentialSkyWalkingLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:                differentialSkyWalkingLoggerCases()[0],
		loggerMethod:          http.MethodPost,
		loggerPath:            differentialSkyWalkingLoggerPath,
		oracleHostWithoutPort: true,
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
				"Content-Type": "application/json",
			}); err != nil {
				return fmt.Errorf("%s SkyWalking headers: %w", side, err)
			}
			if err := validateDifferentialSkyWalkingLoggerPayload(
				call.Body,
				spec.RouteID,
				spec.Steps[0].Request.Path,
			); err != nil {
				return fmt.Errorf("%s SkyWalking payload: %w", side, err)
			}
			call.Body = "skywalking-payload:validated-single-route-format-entry"
			return nil
		},
	})
}

func validateDifferentialSkyWalkingLoggerPayload(body, routeID, endpoint string) error {
	entries, err := decodeDifferentialJSONArray([]byte(body))
	if err != nil {
		return fmt.Errorf("decode entries: %w", err)
	}
	if len(entries) != 1 {
		return fmt.Errorf("entries contain %d values, want exactly one entry", len(entries))
	}

	entry, err := decodeDifferentialJSONObject(
		string(entries[0]),
		map[string]struct{}{
			"body": {}, "service": {}, "serviceInstance": {}, "endpoint": {},
		},
		[]string{"body", "service", "serviceInstance", "endpoint"},
	)
	if err != nil {
		return fmt.Errorf("decode entry: %w", err)
	}
	for name, want := range map[string]string{
		"service": "APISIX", "serviceInstance": "APISIX Instance Name", "endpoint": endpoint,
	} {
		var got string
		if err := json.Unmarshal(entry[name], &got); err != nil {
			return fmt.Errorf("%s is not a string: %w", name, err)
		}
		if got != want {
			return fmt.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	bodyObject, err := decodeDifferentialJSONObject(
		string(entry["body"]),
		map[string]struct{}{"json": {}},
		[]string{"json"},
	)
	if err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	jsonObject, err := decodeDifferentialJSONObject(
		string(bodyObject["json"]),
		map[string]struct{}{"json": {}},
		[]string{"json"},
	)
	if err != nil {
		return fmt.Errorf("decode body.json: %w", err)
	}
	var customPayload string
	if err := json.Unmarshal(jsonObject["json"], &customPayload); err != nil {
		return fmt.Errorf("body.json.json is not a string: %w", err)
	}
	if err := validateDifferentialSkyWalkingLoggerCustomPayload(customPayload, routeID); err != nil {
		return fmt.Errorf("decode body.json.json: %w", err)
	}
	return nil
}

func validateDifferentialSkyWalkingLoggerCustomPayload(body, routeID string) error {
	fields, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{"my_ip": {}, "route_id": {}},
		[]string{"my_ip", "route_id"},
	)
	if err != nil {
		return err
	}
	for name, want := range map[string]string{"my_ip": "127.0.0.1", "route_id": routeID} {
		var got string
		if err := json.Unmarshal(fields[name], &got); err != nil {
			return fmt.Errorf("%s is not a string: %w", name, err)
		}
		if got != want {
			return fmt.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	return nil
}
