package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

func init() {
	differentialComparatorRegistry[differentialKafkaLoggerProducePolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"kafka-logger": {}},
		compare:        compareDifferentialKafkaLoggerProduce,
	}
}

func compareDifferentialKafkaLoggerProduce(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, mustDifferentialCase(spec.Name)) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned kafka-logger case",
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
		if err := normalizeDifferentialKafkaLoggerObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialKafkaLoggerObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if len(observation.Steps) != 0 || observation.Status != spec.Fixture.Response.Status ||
		observation.Body != spec.Fixture.Response.Body || observation.Host != spec.Request.Host ||
		observation.SNI != spec.Request.SNI || observation.SecurityDecision != spec.SecurityDecision {
		return fmt.Errorf("comparison policy %q requires the exact %s gateway response", spec.ComparisonPolicy, side)
	}
	if err := normalizeDifferentialNetworkLoggerGatewayHeaders(
		side,
		observation.Headers,
		len(spec.Fixture.Response.Body),
		"text/plain; charset=utf-8",
		"text/plain; charset=utf-8",
	); err != nil {
		return fmt.Errorf("comparison policy %q %s gateway headers: %w", spec.ComparisonPolicy, side, err)
	}
	if observation.RetryCount != 0 || len(observation.RouteObserver) != 0 ||
		observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" ||
		len(observation.UpstreamCalls) != spec.Fixture.ExpectedCalls ||
		!differentialLoggerUpstreamIsCapturedCall(observation.Upstream, observation.UpstreamCalls) {
		return fmt.Errorf(
			"comparison policy %q requires exactly one %s origin call and one Kafka record",
			spec.ComparisonPolicy,
			side,
		)
	}

	origin := observation.UpstreamCalls[0]
	if !origin.Received || origin.Fixture != spec.Fixture.Name || origin.Method != spec.Request.Method ||
		origin.Path != spec.Request.Path || origin.Host != "differential.example.test" ||
		len(origin.Headers) != 0 || origin.Body != spec.Request.Body {
		return fmt.Errorf("comparison policy %q %s origin request is not exact", spec.ComparisonPolicy, side)
	}
	record := observation.UpstreamCalls[1]
	if !record.Received || record.Fixture != spec.Fixture.Name ||
		record.Method != differentialKafkaMethod || record.Path != "test2" || record.Host != "key1" ||
		len(record.Headers) != 0 {
		return fmt.Errorf("comparison policy %q %s Kafka topic/key envelope is not exact", spec.ComparisonPolicy, side)
	}
	canonical, err := canonicalDifferentialKafkaOriginRecord(record.Body)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s Kafka record: %w", spec.ComparisonPolicy, side, err)
	}
	record.Body = canonical
	observation.UpstreamCalls = []DifferentialUpstreamObservation{origin, record}
	observation.Upstream = record
	return nil
}

type differentialKafkaOriginRecord struct {
	RequestLine string              `json:"request_line"`
	Headers     map[string][]string `json:"headers"`
	Body        string              `json:"body"`
}

func canonicalDifferentialKafkaOriginRecord(raw string) (string, error) {
	parts := strings.Split(raw, "\r\n\r\n")
	if len(parts) != 2 {
		return "", fmt.Errorf("origin record must contain one HTTP header terminator")
	}
	lines := strings.Split(parts[0], "\r\n")
	if len(lines) == 0 || lines[0] != "GET /hello?ab=cd HTTP/1.1" {
		return "", fmt.Errorf("request line = %q, want GET /hello?ab=cd HTTP/1.1", lines[0])
	}
	if parts[1] != "abcdef" {
		return "", fmt.Errorf("request body = %q, want abcdef", parts[1])
	}
	headers := make(map[string][]string, len(lines)-1)
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("malformed origin header %q", line)
		}
		name = strings.ToLower(http.CanonicalHeaderKey(strings.TrimSpace(name)))
		headers[name] = append(headers[name], strings.TrimSpace(value))
	}
	// Connection framing differs because the Oracle request is injected with a
	// one-shot socket while the candidate uses an HTTP transport. It does not
	// change the request represented by the log record.
	delete(headers, "connection")
	for name := range headers {
		sort.Strings(headers[name])
	}
	if values := headers["host"]; len(values) != 1 || values[0] != "localhost" {
		return "", fmt.Errorf("host header = %#v, want localhost", values)
	}
	if values := headers["content-length"]; len(values) != 1 || values[0] != "6" {
		return "", fmt.Errorf("Content-Length header = %#v, want 6", values)
	}
	if values := headers["user-agent"]; len(values) != 1 || values[0] != "Go-http-client/1.1" {
		return "", fmt.Errorf("User-Agent header = %#v, want Go-http-client/1.1", values)
	}
	if values := headers["x-forwarded-host"]; len(values) != 1 || values[0] != "localhost" {
		return "", fmt.Errorf("X-Forwarded-Host header = %#v, want localhost", values)
	}
	if values := headers["x-forwarded-proto"]; len(values) != 1 || values[0] != "http" {
		return "", fmt.Errorf("X-Forwarded-Proto header = %#v, want http", values)
	}
	forwardedPorts := headers["x-forwarded-port"]
	if len(forwardedPorts) != 1 {
		return "", fmt.Errorf("X-Forwarded-Port header = %#v, want one port", forwardedPorts)
	}
	forwardedPort, err := strconv.ParseUint(forwardedPorts[0], 10, 16)
	if err != nil || forwardedPort == 0 {
		return "", fmt.Errorf("X-Forwarded-Port header = %#v, want port 1..65535", forwardedPorts)
	}
	headers["x-forwarded-port"] = []string{"gateway-listener"}
	encoded, err := json.Marshal(differentialKafkaOriginRecord{
		RequestLine: lines[0], Headers: headers, Body: parts[1],
	})
	if err != nil {
		return "", fmt.Errorf("marshal canonical origin record: %w", err)
	}
	return string(encoded), nil
}
