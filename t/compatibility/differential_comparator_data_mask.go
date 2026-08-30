package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
)

const differentialDataMaskRequestLinePolicy = "data-mask-request-line-delivery"

func compareDifferentialDataMaskRequestLine(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(
		spec,
		left,
		right,
		policy,
		differentialLoggerFixtureContract{
			pinned:                mustDifferentialCase(spec.Name),
			loggerMethod:          http.MethodPost,
			loggerPath:            "/logs",
			oracleHostWithoutPort: true,
			validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
				if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
					"Content-Type": "application/json",
				}); err != nil {
					return fmt.Errorf("%s data-mask logger headers: %w", side, err)
				}
				if err := validateDifferentialDataMaskRequestLine(call.Body, spec.RouteID); err != nil {
					return fmt.Errorf("%s data-mask logger payload: %w", side, err)
				}
				call.Body = `{"request_line":"GET /hello?token=***** HTTP/1.1","route_id":"` +
					spec.RouteID + `"}`
				return nil
			},
		},
	)
}

func validateDifferentialDataMaskRequestLine(body string, routeID string) error {
	fields, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{"request_line": {}, "route_id": {}},
		[]string{"request_line", "route_id"},
	)
	if err != nil {
		return err
	}
	var requestLine string
	if err := json.Unmarshal(fields["request_line"], &requestLine); err != nil {
		return fmt.Errorf("request_line is not a string: %w", err)
	}
	const want = "GET /hello?token=***** HTTP/1.1"
	if requestLine != want {
		return fmt.Errorf("request_line = %q, want %q", requestLine, want)
	}
	var gotRouteID string
	if err := json.Unmarshal(fields["route_id"], &gotRouteID); err != nil {
		return fmt.Errorf("route_id is not a string: %w", err)
	}
	if gotRouteID != routeID {
		return fmt.Errorf("route_id = %q, want %q", gotRouteID, routeID)
	}
	return nil
}
