package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

func compareDifferentialSLSLoggerTLSDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialSLSLoggerCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned sls-logger case",
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
		if err := normalizeDifferentialSLSLoggerObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialSLSLoggerObservation(
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
	// The pinned SLS mocking route delimits the Oracle body by closing the
	// HTTP/1.1 connection, while net/http emits an equivalent Content-Length.
	// Canonicalize only that observed absence after the exact body was checked.
	if side == "oracle" && step.Headers != nil &&
		len(differentialHeaderValues(step.Headers, "Content-Length")) == 0 {
		step.Headers["Content-Length"] = []string{strconv.Itoa(len(spec.Fixture.Response.Body))}
	}
	if side == "oracle" {
		date, err := singleDifferentialHeader(step.Headers, "Date")
		if err != nil {
			return fmt.Errorf("comparison policy %q oracle gateway headers Date: %w", spec.ComparisonPolicy, err)
		}
		if _, err := http.ParseTime(date); err != nil {
			return fmt.Errorf(
				"comparison policy %q oracle gateway headers Date = %q: %w",
				spec.ComparisonPolicy,
				date,
				err,
			)
		}
		deleteDifferentialHeader(step.Headers, "Date")
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
			"comparison policy %q %s summary upstream is not the captured TLS frame",
			spec.ComparisonPolicy,
			side,
		)
	}
	raw := observation.UpstreamCalls[0]
	if !raw.Received || raw.Fixture != spec.Fixture.Name || raw.Method != "TCP" || raw.Path != "" ||
		raw.Host != "" || len(raw.Headers) != 0 || raw.Body == "" {
		return fmt.Errorf(
			"comparison policy %q %s raw TLS TCP call must use the TCP marker and empty HTTP metadata",
			spec.ComparisonPolicy, side,
		)
	}
	canonicalFrame, err := validateDifferentialSLSLoggerFrame(
		raw.Body, spec.RouteID, wantStep.Request.Host,
	)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s RFC5424 frame: %w", spec.ComparisonPolicy, side, err)
	}
	raw.Body = canonicalFrame
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	observation.UpstreamCalls = []DifferentialUpstreamObservation{raw}
	observation.Upstream = raw
	return nil
}

func validateDifferentialSLSLoggerFrame(body string, routeID string, hostname string) (string, error) {
	if strings.Count(body, "\n") != 1 || !strings.HasSuffix(body, "\n") {
		return "", fmt.Errorf("payload is not a single newline-terminated RFC5424 frame")
	}
	parts := strings.SplitN(strings.TrimSuffix(body, "\n"), " ", 7)
	if len(parts) != 7 || parts[0] != "<46>1" || parts[3] != "apisix" || parts[5] != "-" {
		return "", fmt.Errorf("payload does not have the exact APISIX 3.17 RFC5424 envelope")
	}
	if !differentialRFC5424MillisTimestampPattern.MatchString(parts[1]) {
		return "", fmt.Errorf("timestamp %q is not an APISIX 3.17 millisecond UTC timestamp", parts[1])
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", parts[1]); err != nil {
		return "", fmt.Errorf("timestamp %q is not an APISIX 3.17 millisecond UTC timestamp", parts[1])
	}
	if parts[2] != hostname {
		return "", fmt.Errorf("hostname = %q, want request hostname %q", parts[2], hostname)
	}
	if pid, err := strconv.Atoi(parts[4]); err != nil || pid <= 0 {
		return "", fmt.Errorf("PID = %q, want a positive integer", parts[4])
	}
	wantStructuredData := differentialSLSLoggerStructuredData()
	if !strings.HasPrefix(parts[6], wantStructuredData+" ") {
		return "", fmt.Errorf("structured data does not match the pinned project, logstore, and credentials")
	}
	payload := strings.TrimPrefix(parts[6], wantStructuredData+" ")
	canonicalPayload, err := validateDifferentialSLSLoggerPayload(payload, routeID)
	if err != nil {
		return "", err
	}
	return "<46>1 <timestamp> " + hostname + " apisix <pid> - " +
		wantStructuredData + " " + canonicalPayload + "\n", nil
}

func differentialSLSLoggerStructuredData() string {
	return `[logservice project="` + differentialSLSLoggerProject +
		`" logstore="` + differentialSLSLoggerLogstore +
		`" access-key-id="` + differentialSLSLoggerAccessKeyID +
		`" access-key-secret="` + differentialSLSLoggerAccessKeySecret + `"]`
}

func validateDifferentialSLSLoggerPayload(body string, routeID string) (string, error) {
	fields, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{"case": {}, "route_id": {}},
		[]string{"case", "route_id"},
	)
	if err != nil {
		return "", err
	}
	var customCase string
	if err := json.Unmarshal(fields["case"], &customCase); err != nil || customCase != "sls-logger" {
		return "", fmt.Errorf("field case = %q, want sls-logger: %v", customCase, err)
	}
	var gotRouteID string
	if err := json.Unmarshal(fields["route_id"], &gotRouteID); err != nil || gotRouteID != routeID {
		return "", fmt.Errorf("field route_id = %q, want %q: %v", gotRouteID, routeID, err)
	}
	return `{"case":"sls-logger","route_id":"` + routeID + `"}`, nil
}
