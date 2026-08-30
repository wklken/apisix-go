package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const differentialSyslogGatewayPath = "/logger/syslog"

var differentialSyslogRFC5424Pattern = regexp.MustCompile(
	`^<46>1 ([^ ]+) ([^ ]+) apisix ([1-9][0-9]*) - - (.+)$`,
)

var differentialRFC5424MillisTimestampPattern = regexp.MustCompile(
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`,
)

func compareDifferentialSyslogTCPDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, mustDifferentialCase(spec.Name)) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned syslog case",
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
		if err := normalizeDifferentialSyslogObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialSyslogObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if len(observation.Steps) != 1 {
		return fmt.Errorf("comparison policy %q requires one %s gateway step", spec.ComparisonPolicy, side)
	}
	step := observation.Steps[0]
	wantStep := spec.Steps[0]
	if step.Status != spec.Fixture.Response.Status ||
		step.Body != spec.Fixture.Response.Body || step.Host != wantStep.Request.Host ||
		step.SNI != wantStep.Request.SNI || step.SecurityDecision != wantStep.SecurityDecision {
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

	origin, raw, err := differentialSyslogCalls(spec, side, observation.UpstreamCalls)
	if err != nil {
		return err
	}
	if origin.Host != "differential.example.test" || len(origin.Headers) != 0 || origin.Body != "" {
		return fmt.Errorf("comparison policy %q %s origin request is not the pinned GET", spec.ComparisonPolicy, side)
	}
	canonicalFrame, err := validateDifferentialSyslogFrame(raw.Body, spec.RouteID, wantStep.Request.Host)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s RFC5424 frame: %w", spec.ComparisonPolicy, side, err)
	}

	origin.Path = differentialSyslogGatewayPath
	raw.Body = canonicalFrame
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	observation.UpstreamCalls = []DifferentialUpstreamObservation{origin, raw}
	observation.Upstream = raw
	return nil
}

func differentialSyslogCalls(
	spec DifferentialCase,
	side string,
	calls []DifferentialUpstreamObservation,
) (DifferentialUpstreamObservation, DifferentialUpstreamObservation, error) {
	var origin DifferentialUpstreamObservation
	var raw DifferentialUpstreamObservation
	for _, call := range calls {
		if !call.Received || call.Fixture != spec.Fixture.Name {
			return origin, raw, fmt.Errorf(
				"comparison policy %q %s fixture call identity is invalid", spec.ComparisonPolicy, side,
			)
		}
		switch {
		case call.Method == http.MethodGet &&
			differentialLoggerRequestTargetMatches(call.Path, differentialSyslogGatewayPath):
			if origin.Received {
				return origin, raw, fmt.Errorf(
					"comparison policy %q %s has duplicate origin GET",
					spec.ComparisonPolicy,
					side,
				)
			}
			origin = call
		case call.Method == "TCP" && call.Path == "" && call.Host == "" && len(call.Headers) == 0:
			if raw.Received {
				return origin, raw, fmt.Errorf(
					"comparison policy %q %s has duplicate raw TCP call",
					spec.ComparisonPolicy,
					side,
				)
			}
			raw = call
		default:
			return origin, raw, fmt.Errorf(
				"comparison policy %q %s has an unapproved call; raw TCP call must use the TCP marker and empty HTTP metadata",
				spec.ComparisonPolicy,
				side,
			)
		}
	}
	if !origin.Received {
		return origin, raw, fmt.Errorf("comparison policy %q %s is missing the origin GET", spec.ComparisonPolicy, side)
	}
	if !raw.Received || raw.Body == "" {
		return origin, raw, fmt.Errorf(
			"comparison policy %q %s is missing the raw TCP call",
			spec.ComparisonPolicy,
			side,
		)
	}
	return origin, raw, nil
}

func validateDifferentialSyslogFrame(body string, routeID string, hostname string) (string, error) {
	if strings.Count(body, "\n") != 1 || !strings.HasSuffix(body, "\n") {
		return "", fmt.Errorf("payload is not a single newline-terminated RFC5424 frame")
	}
	matches := differentialSyslogRFC5424Pattern.FindStringSubmatch(strings.TrimSuffix(body, "\n"))
	if len(matches) != 5 {
		return "", fmt.Errorf("payload does not have the exact APISIX 3.17 RFC5424 envelope")
	}
	if !differentialRFC5424MillisTimestampPattern.MatchString(matches[1]) {
		return "", fmt.Errorf("timestamp %q is not an APISIX 3.17 millisecond UTC timestamp", matches[1])
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", matches[1]); err != nil {
		return "", fmt.Errorf("timestamp %q is not an APISIX 3.17 millisecond UTC timestamp", matches[1])
	}
	if matches[2] != hostname {
		return "", fmt.Errorf("hostname = %q, want request hostname %q", matches[2], hostname)
	}
	if pid, err := strconv.Atoi(matches[3]); err != nil || pid <= 0 {
		return "", fmt.Errorf("PID = %q, want a positive integer", matches[3])
	}
	canonicalPayload, err := validateDifferentialSyslogPayload(matches[4], routeID)
	if err != nil {
		return "", err
	}
	return "<46>1 <timestamp> " + hostname + " apisix <pid> - - " + canonicalPayload + "\n", nil
}

func validateDifferentialSyslogPayload(body string, routeID string) (string, error) {
	fields, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{"case": {}, "route_id": {}},
		[]string{"case", "route_id"},
	)
	if err != nil {
		return "", err
	}
	var customCase string
	if err := json.Unmarshal(fields["case"], &customCase); err != nil || customCase != "syslog" {
		return "", fmt.Errorf("field case = %q, want syslog: %v", customCase, err)
	}
	var gotRouteID string
	if err := json.Unmarshal(fields["route_id"], &gotRouteID); err != nil || gotRouteID != routeID {
		return "", fmt.Errorf("field route_id = %q, want %q: %v", gotRouteID, routeID, err)
	}
	return `{"case":"syslog","route_id":"` + routeID + `"}`, nil
}
