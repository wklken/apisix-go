package pluginintegration

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

func init() {
	differentialComparatorRegistry[differentialSkyWalkingSW8FullSamplingPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"skywalking": {}},
		compare:        compareDifferentialSkyWalkingSW8FullSampling,
	}
}

func compareDifferentialSkyWalkingSW8FullSampling(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, mustDifferentialCase(spec.Name)) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned SkyWalking case",
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
		if err := normalizeDifferentialSkyWalkingSW8Observation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialSkyWalkingSW8Observation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
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
	if observation.Status != 0 || len(observation.Headers) != 0 || observation.Body != "" ||
		observation.Host != "" || observation.SNI != "" || observation.SecurityDecision != "" ||
		observation.RetryCount != 0 || len(observation.RouteObserver) != 0 || observation.File != nil {
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
			spec.ComparisonPolicy, spec.Fixture.ExpectedCalls, side,
		)
	}
	if !differentialLoggerUpstreamIsCapturedCall(observation.Upstream, observation.UpstreamCalls) {
		return fmt.Errorf(
			"comparison policy %q %s summary upstream is not a captured call",
			spec.ComparisonPolicy,
			side,
		)
	}

	call := observation.UpstreamCalls[0]
	if !call.Received || call.Fixture != spec.Fixture.Name || call.Method != http.MethodGet ||
		!differentialLoggerRequestTargetMatches(call.Path, wantStep.Request.Path) ||
		call.Host != "differential.example.test" || call.Body != "" {
		return fmt.Errorf("comparison policy %q %s origin request is not the pinned GET", spec.ComparisonPolicy, side)
	}
	sw8, err := singleDifferentialSW8Header(call.Headers)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s origin SW8: %w", spec.ComparisonPolicy, side, err)
	}
	canonical, err := validateAndNormalizeDifferentialSW8(sw8)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s origin SW8: %w", spec.ComparisonPolicy, side, err)
	}

	call.Path = wantStep.Request.Path
	call.Headers = map[string][]string{"Sw8": {canonical}}
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	observation.UpstreamCalls = []DifferentialUpstreamObservation{call}
	observation.Upstream = call
	return nil
}

func singleDifferentialSW8Header(headers map[string][]string) (string, error) {
	if len(headers) != 1 {
		return "", fmt.Errorf("header count = %d, want only sw8", len(headers))
	}
	for name, values := range headers {
		if !strings.EqualFold(name, "sw8") {
			return "", fmt.Errorf("unexpected semantic header %q", name)
		}
		if len(values) != 1 || values[0] == "" {
			return "", fmt.Errorf("sw8 must have exactly one non-empty value")
		}
		return values[0], nil
	}
	return "", fmt.Errorf("sw8 header is missing")
}

func validateAndNormalizeDifferentialSW8(value string) (string, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 8 {
		return "", fmt.Errorf("SW8 has %d fields, want exactly 8 fields", len(parts))
	}
	if parts[0] != "1" {
		return "", fmt.Errorf("SW8 sample flag = %q, want 1", parts[0])
	}
	decoded := make(map[int][]byte, 6)
	for _, index := range []int{1, 2, 4, 5, 6, 7} {
		value, err := decodeNonEmptyDifferentialSW8Field(parts[index])
		if err != nil {
			return "", fmt.Errorf("SW8 field %d is not non-empty base64url: %w", index+1, err)
		}
		decoded[index] = value
	}
	spanID, err := strconv.Atoi(parts[3])
	if err != nil {
		return "", fmt.Errorf("SW8 span id %q is not an integer", parts[3])
	}
	if spanID != 1 {
		return "", fmt.Errorf("SW8 span id = %d, want APISIX 3.17 exit span 1", spanID)
	}
	for index, expected := range map[int]string{
		4: "APISIX",
		5: "APISIX Instance Name",
		6: "/opentracing",
		7: "upstream service",
	} {
		if actual := string(decoded[index]); actual != expected {
			field := map[int]string{4: "service", 5: "instance", 6: "operation", 7: "peer service"}[index]
			return "", fmt.Errorf("SW8 %s = %q, want %q", field, actual, expected)
		}
		parts[index] = base64.RawURLEncoding.EncodeToString(decoded[index])
	}
	parts[1] = "<validated-random-trace-id>"
	parts[2] = "<validated-random-segment-id>"
	return strings.Join(parts, "-"), nil
}

func decodeNonEmptyDifferentialSW8Field(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("empty value")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(value)
	}
	if err != nil {
		return nil, err
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("decoded value is empty")
	}
	return decoded, nil
}
