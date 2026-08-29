package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const differentialErrorLogLoggerWarning = "Invalid authorization header format"

var differentialErrorLogLoggerOracleLinePattern = regexp.MustCompile(
	`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) \[warn\] \d+#\d+: \*\d+ ` +
		`\[lua\] basic-auth\.lua:136: find_consumer\(\): ` +
		regexp.QuoteMeta(differentialErrorLogLoggerWarning) +
		`, client: ([^,]+), server: _, request: "GET /warn HTTP/1\.1", ` +
		`host: "gateway\.example\.test", request_id: "([0-9a-f]{32})"$`,
)

func compareDifferentialErrorLogLoggerClickHouseDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	pinned := differentialErrorLogLoggerCases()[0]
	if !reflect.DeepEqual(spec, pinned) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned error-log-logger case",
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
		if err := normalizeDifferentialErrorLogLoggerObservation(
			spec, side.name, side.observation,
		); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialErrorLogLoggerObservation(
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
	if err := validateDifferentialErrorLogLoggerGatewayHeaders(side, step.Headers); err != nil {
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

	originIndex := differentialErrorLogLoggerCallIndex(
		observation.UpstreamCalls, http.MethodGet, wantStep.Request.Path,
	)
	if originIndex < 0 {
		return fmt.Errorf(
			"comparison policy %q %s fixture calls are missing GET %s",
			spec.ComparisonPolicy,
			side,
			wantStep.Request.Path,
		)
	}
	loggerIndices := differentialErrorLogLoggerCallIndices(
		observation.UpstreamCalls, http.MethodPost, differentialErrorLogLoggerClickHousePath,
	)
	if len(loggerIndices) != 1 {
		return fmt.Errorf(
			"comparison policy %q %s fixture calls require exactly one POST %s entry",
			spec.ComparisonPolicy, side, differentialErrorLogLoggerClickHousePath,
		)
	}

	origin := observation.UpstreamCalls[originIndex]
	if !origin.Received || origin.Fixture != spec.Fixture.Name ||
		origin.Host != "differential.example.test" || origin.Body != "" {
		return fmt.Errorf("comparison policy %q %s origin request is not the pinned call", spec.ComparisonPolicy, side)
	}
	if err := validateDifferentialLoggerHeaders(origin.Headers, map[string]string{
		"X-Consumer-Username": "anonymous",
	}); err != nil {
		return fmt.Errorf("comparison policy %q %s origin headers: %w", spec.ComparisonPolicy, side, err)
	}

	loggerCall := observation.UpstreamCalls[loggerIndices[0]]
	if !loggerCall.Received || loggerCall.Fixture != spec.Fixture.Name {
		return fmt.Errorf("comparison policy %q %s logger fixture identity is invalid", spec.ComparisonPolicy, side)
	}
	if err := validateDifferentialLoggerFixtureHost(
		side, loggerCall.Host, observation.UpstreamAddress, false,
	); err != nil {
		return fmt.Errorf("comparison policy %q %s logger fixture Host: %w", spec.ComparisonPolicy, side, err)
	}
	if err := validateDifferentialLoggerHeaders(loggerCall.Headers, map[string]string{
		"Content-Type":          "application/json",
		"X-ClickHouse-User":     "default",
		"X-ClickHouse-Key":      "differential-password",
		"X-ClickHouse-Database": "default",
	}); err != nil {
		return fmt.Errorf("comparison policy %q %s ClickHouse headers: %w", spec.ComparisonPolicy, side, err)
	}
	line, err := decodeDifferentialErrorLogLoggerClickHouseLine(loggerCall.Body)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s ClickHouse payload: %w", spec.ComparisonPolicy, side, err)
	}
	if err := validateDifferentialErrorLogLoggerWarningLine(side, line); err != nil {
		return fmt.Errorf("comparison policy %q %s ClickHouse warning: %w", spec.ComparisonPolicy, side, err)
	}

	origin.Path = wantStep.Request.Path
	loggerCall.Path = differentialErrorLogLoggerClickHousePath
	loggerCall.Host = "fixture:" + spec.Fixture.Name
	loggerCall.Body = `INSERT INTO logs FORMAT JSONEachRow {"data":"[warn] Invalid authorization header format"}`
	observation.UpstreamCalls = []DifferentialUpstreamObservation{origin, loggerCall}
	observation.Upstream = loggerCall
	return nil
}

func validateDifferentialErrorLogLoggerGatewayHeaders(
	side string,
	headers map[string][]string,
) error {
	for name := range headers {
		switch strings.ToLower(http.CanonicalHeaderKey(name)) {
		case "content-length", "date", "server":
		default:
			return fmt.Errorf("unapproved gateway response header %q", name)
		}
	}
	server, err := singleDifferentialHeader(headers, "Server")
	if err != nil {
		return err
	}
	switch side {
	case "candidate":
		if !strings.HasPrefix(server, "APISIX/") {
			return fmt.Errorf("candidate Server = %q, want APISIX identity", server)
		}
		date, err := singleDifferentialHeader(headers, "Date")
		if err != nil {
			return err
		}
		if _, err := http.ParseTime(date); err != nil {
			return fmt.Errorf("candidate Date = %q: %w", date, err)
		}
	case "oracle":
		if server != "APISIX/3.17.0" {
			return fmt.Errorf("oracle Server = %q, want APISIX/3.17.0", server)
		}
		contentLength, err := singleDifferentialHeader(headers, "Content-Length")
		if err != nil {
			return err
		}
		if contentLength != "0" {
			return fmt.Errorf("oracle Content-Length = %q, want 0", contentLength)
		}
	default:
		return fmt.Errorf("unknown observation side %q", side)
	}
	return nil
}

func differentialErrorLogLoggerCallIndex(
	calls []DifferentialUpstreamObservation,
	method string,
	path string,
) int {
	for index := range calls {
		if calls[index].Method == method && calls[index].Path == path {
			return index
		}
	}
	return -1
}

func differentialErrorLogLoggerCallIndices(
	calls []DifferentialUpstreamObservation,
	method string,
	path string,
) []int {
	indices := make([]int, 0, len(calls))
	for index := range calls {
		if calls[index].Method == method && calls[index].Path == path {
			indices = append(indices, index)
		}
	}
	return indices
}

func decodeDifferentialErrorLogLoggerClickHouseLine(body string) (string, error) {
	const prefix = "INSERT INTO logs FORMAT JSONEachRow "
	if !strings.HasPrefix(body, prefix) {
		return "", fmt.Errorf("body does not have the exact ClickHouse INSERT prefix")
	}
	fields, err := decodeDifferentialJSONObject(
		strings.TrimPrefix(body, prefix),
		map[string]struct{}{"data": {}},
		[]string{"data"},
	)
	if err != nil {
		return "", err
	}
	var line string
	if err := json.Unmarshal(fields["data"], &line); err != nil {
		return "", fmt.Errorf("field data is not a string: %w", err)
	}
	return line, nil
}

func validateDifferentialErrorLogLoggerWarningLine(side string, line string) error {
	switch side {
	case "candidate":
		const marker = " [warn] " + differentialErrorLogLoggerWarning
		if !strings.HasSuffix(line, marker) {
			return fmt.Errorf("candidate log line does not contain the exact WARN message")
		}
		timestamp := strings.TrimSuffix(line, marker)
		if parsed, err := time.Parse(time.RFC3339Nano, timestamp); err != nil || parsed.Location() != time.UTC {
			return fmt.Errorf("candidate log line timestamp %q is not UTC RFC3339Nano", timestamp)
		}
	case "oracle":
		matches := differentialErrorLogLoggerOracleLinePattern.FindStringSubmatch(line)
		if len(matches) != 4 {
			return fmt.Errorf("oracle log line does not match the exact NGINX WARN request record")
		}
		if _, err := time.Parse("2006/01/02 15:04:05", matches[1]); err != nil {
			return fmt.Errorf("oracle log line timestamp %q is invalid", matches[1])
		}
		if net.ParseIP(matches[2]) == nil {
			return fmt.Errorf("oracle log line client %q is not an IP address", matches[2])
		}
	default:
		return fmt.Errorf("unknown observation side %q", side)
	}
	return nil
}
