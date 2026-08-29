package pluginintegration

import (
	"encoding/json"
	"fmt"
	"reflect"
)

const differentialTCPLoggerFixtureMethod = "TCP"

type differentialRawLoggerFixtureContract struct {
	pinned       DifferentialCase
	rawMethod    string
	validateBody func(string) (string, error)
}

func compareDifferentialTCPLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialRawLoggerFixtureDelivery(spec, left, right, policy, differentialRawLoggerFixtureContract{
		pinned:    mustDifferentialCase(spec.Name),
		rawMethod: differentialTCPLoggerFixtureMethod,
		validateBody: func(body string) (string, error) {
			if err := validateDifferentialTCPLoggerPayload(body, spec.RouteID); err != nil {
				return "", err
			}
			return "tcp-json-frame:validated-single-object", nil
		},
	})
}

func compareDifferentialRawLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
	contract differentialRawLoggerFixtureContract,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, contract.pinned) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned %s case",
			spec.ComparisonPolicy,
			contract.pinned.Plugin,
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
		if err := normalizeDifferentialRawLoggerFixtureObservation(
			spec, side.name, side.observation, contract,
		); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialRawLoggerFixtureObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
	contract differentialRawLoggerFixtureContract,
) error {
	if len(observation.Steps) != 1 {
		return fmt.Errorf("comparison policy %q requires one %s gateway step", spec.ComparisonPolicy, side)
	}
	step := observation.Steps[0]
	wantStep := spec.Steps[0]
	if step.Status != spec.Fixture.Response.Status || step.Body != spec.Fixture.Response.Body ||
		step.Host != wantStep.Request.Host || step.SNI != wantStep.Request.SNI ||
		step.SecurityDecision != wantStep.SecurityDecision {
		return fmt.Errorf("comparison policy %q requires the exact %s gateway step", spec.ComparisonPolicy, side)
	}
	if err := normalizeDifferentialNetworkLoggerGatewayHeaders(
		side,
		step.Headers,
		len(spec.Fixture.Response.Body),
		"text/plain; charset=utf-8",
		"text/plain; charset=utf-8",
	); err != nil {
		return fmt.Errorf("comparison policy %q %s gateway headers: %w", spec.ComparisonPolicy, side, err)
	}
	if observation.Status != 0 || len(observation.Headers) != 0 || observation.Body != "" ||
		observation.Host != "" || observation.SNI != "" || observation.SecurityDecision != "" ||
		observation.RetryCount != 0 || len(observation.RouteObserver) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires the sequence-only %s observation envelope",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" ||
		!observation.Upstream.Received || observation.Upstream.Fixture != spec.Fixture.Name ||
		len(observation.UpstreamCalls) != spec.Fixture.ExpectedCalls {
		return fmt.Errorf(
			"comparison policy %q requires exactly %d identified %s fixture calls",
			spec.ComparisonPolicy,
			spec.Fixture.ExpectedCalls,
			side,
		)
	}
	if !differentialLoggerUpstreamIsCapturedCall(observation.Upstream, observation.UpstreamCalls) {
		return fmt.Errorf(
			"comparison policy %q %s summary upstream is not a captured call",
			spec.ComparisonPolicy,
			side,
		)
	}

	originIndex := -1
	rawIndex := -1
	for index, call := range observation.UpstreamCalls {
		if !call.Received || call.Fixture != spec.Fixture.Name {
			return fmt.Errorf("comparison policy %q %s fixture call identity is invalid", spec.ComparisonPolicy, side)
		}
		switch {
		case call.Method == wantStep.Request.Method && call.Path == wantStep.Request.Path:
			if originIndex >= 0 {
				return fmt.Errorf(
					"comparison policy %q %s contains multiple origin requests",
					spec.ComparisonPolicy,
					side,
				)
			}
			originIndex = index
		case call.Method == contract.rawMethod && call.Path == "":
			if rawIndex >= 0 {
				return fmt.Errorf(
					"comparison policy %q %s contains multiple %s messages",
					spec.ComparisonPolicy,
					side,
					contract.rawMethod,
				)
			}
			rawIndex = index
		default:
			return fmt.Errorf(
				"comparison policy %q %s contains unexpected fixture call %s %s",
				spec.ComparisonPolicy,
				side,
				call.Method,
				call.Path,
			)
		}
	}
	if originIndex < 0 || rawIndex < 0 {
		return fmt.Errorf(
			"comparison policy %q %s is missing one origin request and one %s message",
			spec.ComparisonPolicy,
			side,
			contract.rawMethod,
		)
	}
	origin := observation.UpstreamCalls[originIndex]
	if origin.Host != "differential.example.test" || len(origin.Headers) != 0 || origin.Body != "" {
		return fmt.Errorf("comparison policy %q %s origin request is not exact", spec.ComparisonPolicy, side)
	}
	raw := observation.UpstreamCalls[rawIndex]
	if raw.Host != "" || len(raw.Headers) != 0 || raw.Body == "" {
		return fmt.Errorf(
			"comparison policy %q %s %s message envelope is not exact",
			spec.ComparisonPolicy,
			side,
			contract.rawMethod,
		)
	}
	canonicalBody, err := contract.validateBody(raw.Body)
	if err != nil {
		return fmt.Errorf(
			"comparison policy %q %s %s payload: %w",
			spec.ComparisonPolicy,
			side,
			contract.rawMethod,
			err,
		)
	}
	raw.Body = canonicalBody
	observation.UpstreamCalls = []DifferentialUpstreamObservation{origin, raw}
	observation.Upstream = raw
	return nil
}

func validateDifferentialTCPLoggerPayload(body, routeID string) error {
	fields, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{
			"case name": {}, "vip": {}, "status": {}, "route_id": {},
		},
		[]string{"case name", "vip", "status", "route_id"},
	)
	if err != nil {
		return err
	}
	if err := validateDifferentialRawLoggerStringFields(fields, map[string]string{
		"case name": "logger format in plugin",
		"vip":       "127.0.0.1",
		"route_id":  routeID,
	}); err != nil {
		return err
	}
	var status int
	if err := json.Unmarshal(fields["status"], &status); err != nil {
		return fmt.Errorf("status is not a number: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("status = %d, want 200", status)
	}
	return nil
}

func validateDifferentialRawLoggerStringFields(
	fields map[string]json.RawMessage,
	want map[string]string,
) error {
	for name, wantValue := range want {
		var got string
		if err := json.Unmarshal(fields[name], &got); err != nil {
			return fmt.Errorf("%s is not a string: %w", name, err)
		}
		if got != wantValue {
			return fmt.Errorf("%s = %q, want %q", name, got, wantValue)
		}
	}
	return nil
}
