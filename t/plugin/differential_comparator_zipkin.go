package pluginintegration

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"reflect"
	"strings"
)

type differentialZipkinSpan struct {
	TraceID        string                      `json:"traceId"`
	Name           string                      `json:"name"`
	ParentID       string                      `json:"parentId,omitempty"`
	ID             string                      `json:"id"`
	Kind           string                      `json:"kind,omitempty"`
	Timestamp      json.Number                 `json:"timestamp"`
	Duration       json.Number                 `json:"duration"`
	LocalEndpoint  *differentialZipkinEndpoint `json:"localEndpoint"`
	RemoteEndpoint *differentialZipkinEndpoint `json:"remoteEndpoint,omitempty"`
	Tags           map[string]string           `json:"tags"`
	Annotations    json.RawMessage             `json:"annotations,omitempty"`
}

type differentialZipkinEndpoint struct {
	ServiceName string `json:"serviceName,omitempty"`
	IPv4        string `json:"ipv4,omitempty"`
	IPv6        string `json:"ipv6,omitempty"`
	Port        int    `json:"port,omitempty"`
}

func compareDifferentialZipkinV2ServerSpanCore(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialZipkinCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned Zipkin v2 case",
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
		if err := normalizeDifferentialZipkinV2Observation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialZipkinV2Observation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if len(observation.Steps) != 1 {
		return fmt.Errorf("comparison policy %q requires one %s gateway step", spec.ComparisonPolicy, side)
	}
	step := observation.Steps[0]
	wantStep := spec.Steps[0]
	if step.Status != http.StatusOK || step.Body != spec.Fixture.Response.Body ||
		step.Host != wantStep.Request.Host || step.SNI != wantStep.Request.SNI ||
		step.SecurityDecision != wantStep.SecurityDecision {
		return fmt.Errorf("comparison policy %q requires the exact %s gateway step", spec.ComparisonPolicy, side)
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

	origin, collector, err := differentialZipkinCalls(spec, side, observation.UpstreamCalls)
	if err != nil {
		return err
	}
	if origin.Host != "differential.example.test" || origin.Body != "" {
		return fmt.Errorf("comparison policy %q %s origin request is not the pinned GET", spec.ComparisonPolicy, side)
	}
	if err := validateDifferentialLoggerFixtureHost(
		side,
		collector.Host,
		observation.UpstreamAddress,
		false,
	); err != nil {
		return fmt.Errorf("comparison policy %q %s collector Host: %w", spec.ComparisonPolicy, side, err)
	}
	if err := validateDifferentialLoggerHeaders(collector.Headers, map[string]string{
		"Content-Type": "application/json",
	}); err != nil {
		return fmt.Errorf("comparison policy %q %s collector headers: %w", spec.ComparisonPolicy, side, err)
	}

	spans, err := decodeDifferentialZipkinSpans(collector.Body)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s collector body: %w", spec.ComparisonPolicy, side, err)
	}
	server, forwardID, forwardParentID, err := validateDifferentialZipkinTopology(side, spans)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s spans: %w", spec.ComparisonPolicy, side, err)
	}
	if err := validateDifferentialZipkinOriginHeaders(origin.Headers, forwardID, forwardParentID); err != nil {
		return fmt.Errorf("comparison policy %q %s origin B3: %w", spec.ComparisonPolicy, side, err)
	}

	origin.Path = spec.Steps[0].Request.Path
	origin.Headers = map[string][]string{
		"X-B3-Flags":        {"1"},
		"X-B3-ParentSpanId": {"<validated-parent>"},
		"X-B3-Sampled":      {"1"},
		"X-B3-SpanId":       {"<validated-forward-span>"},
		"X-B3-TraceId":      {differentialZipkinTraceID},
	}
	collector.Path = differentialZipkinCollectorPath
	collector.Host = "fixture:" + spec.Fixture.Name
	collector.Body = differentialZipkinCanonicalServerSpan(server)
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	observation.UpstreamCalls = []DifferentialUpstreamObservation{origin, collector}
	observation.Upstream = collector
	return nil
}

func differentialZipkinCalls(
	spec DifferentialCase,
	side string,
	calls []DifferentialUpstreamObservation,
) (DifferentialUpstreamObservation, DifferentialUpstreamObservation, error) {
	var origin DifferentialUpstreamObservation
	var collector DifferentialUpstreamObservation
	for _, call := range calls {
		if !call.Received || call.Fixture != spec.Fixture.Name {
			return origin, collector, fmt.Errorf(
				"comparison policy %q %s fixture call identity is invalid",
				spec.ComparisonPolicy,
				side,
			)
		}
		switch {
		case call.Method == http.MethodGet &&
			differentialLoggerRequestTargetMatches(call.Path, spec.Steps[0].Request.Path):
			if origin.Received {
				return origin, collector, fmt.Errorf(
					"comparison policy %q %s has duplicate origin GET",
					spec.ComparisonPolicy,
					side,
				)
			}
			origin = call
		case call.Method == http.MethodPost &&
			differentialLoggerRequestTargetMatches(call.Path, differentialZipkinCollectorPath):
			if collector.Received {
				return origin, collector, fmt.Errorf(
					"comparison policy %q %s has duplicate collector POST",
					spec.ComparisonPolicy,
					side,
				)
			}
			collector = call
		}
	}
	if !origin.Received {
		return origin, collector, fmt.Errorf(
			"comparison policy %q %s fixture calls are missing GET %s",
			spec.ComparisonPolicy,
			side,
			spec.Steps[0].Request.Path,
		)
	}
	if !collector.Received {
		return origin, collector, fmt.Errorf(
			"comparison policy %q %s fixture calls are missing POST %s",
			spec.ComparisonPolicy,
			side,
			differentialZipkinCollectorPath,
		)
	}
	return origin, collector, nil
}

func decodeDifferentialZipkinSpans(body string) ([]differentialZipkinSpan, error) {
	var spans []differentialZipkinSpan
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&spans); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("contains more than one JSON value")
		}
		return nil, err
	}
	if spans == nil {
		return nil, fmt.Errorf("collector body is not a span array")
	}
	return spans, nil
}

func validateDifferentialZipkinTopology(
	side string,
	spans []differentialZipkinSpan,
) (differentialZipkinSpan, string, string, error) {
	wantCount := 1
	if side == "oracle" {
		wantCount = 3
	}
	if len(spans) != wantCount {
		if side == "oracle" {
			return differentialZipkinSpan{}, "", "", fmt.Errorf(
				"collector must contain exactly three v2 spans; v1 phase topology is out of scope",
			)
		}
		return differentialZipkinSpan{}, "", "", fmt.Errorf("collector must contain exactly one SERVER span")
	}
	byName := make(map[string]differentialZipkinSpan, len(spans))
	for _, span := range spans {
		if _, exists := byName[span.Name]; exists {
			return differentialZipkinSpan{}, "", "", fmt.Errorf("duplicate span name %q", span.Name)
		}
		byName[span.Name] = span
	}
	server, exists := byName["apisix.request"]
	if !exists {
		return differentialZipkinSpan{}, "", "", fmt.Errorf("missing apisix.request SERVER span")
	}
	if err := validateDifferentialZipkinServerSpan(server); err != nil {
		return differentialZipkinSpan{}, "", "", err
	}

	forwardID := server.ID
	forwardParentID := differentialZipkinIncomingSpanID
	if side == "oracle" {
		proxy, proxyExists := byName["apisix.proxy"]
		response, responseExists := byName["apisix.response_span"]
		if !proxyExists || !responseExists {
			return differentialZipkinSpan{}, "", "", fmt.Errorf(
				"oracle is missing the exact v2 proxy/response topology",
			)
		}
		for _, child := range []differentialZipkinSpan{proxy, response} {
			if err := validateDifferentialZipkinPhaseSpan(child, server.ID); err != nil {
				return differentialZipkinSpan{}, "", "", err
			}
		}
		forwardID = proxy.ID
		forwardParentID = server.ID
	}
	return server, forwardID, forwardParentID, nil
}

func validateDifferentialZipkinServerSpan(span differentialZipkinSpan) error {
	if span.TraceID != differentialZipkinTraceID {
		return fmt.Errorf("SERVER traceId = %q, want incoming trace", span.TraceID)
	}
	if span.ParentID != differentialZipkinIncomingSpanID {
		return fmt.Errorf(
			"SERVER parentId = %q, want incoming span %q",
			span.ParentID,
			differentialZipkinIncomingSpanID,
		)
	}
	if !validDifferentialZipkinID(span.ID, 8) {
		return fmt.Errorf("SERVER id = %q, want a random 16-digit lowercase hex ID", span.ID)
	}
	if span.Kind != "SERVER" {
		return fmt.Errorf("SERVER kind = %q", span.Kind)
	}
	if err := validateDifferentialZipkinTiming(span); err != nil {
		return err
	}
	if err := validateDifferentialZipkinLocalEndpoint(span.LocalEndpoint); err != nil {
		return err
	}
	if err := validateDifferentialZipkinRemoteEndpoint(span.RemoteEndpoint); err != nil {
		return err
	}
	if err := validateDifferentialZipkinServerTags(span.Tags); err != nil {
		return err
	}
	return validateDifferentialZipkinAnnotations(span.Annotations)
}

func validateDifferentialZipkinPhaseSpan(span differentialZipkinSpan, serverID string) error {
	if span.TraceID != differentialZipkinTraceID || span.ParentID != serverID ||
		!validDifferentialZipkinID(span.ID, 8) || span.Kind != "" {
		return fmt.Errorf("v2 phase span %q does not preserve the request-span parent relationship", span.Name)
	}
	if err := validateDifferentialZipkinTiming(span); err != nil {
		return fmt.Errorf("v2 phase span %q: %w", span.Name, err)
	}
	if err := validateDifferentialZipkinLocalEndpoint(span.LocalEndpoint); err != nil {
		return fmt.Errorf("v2 phase span %q: %w", span.Name, err)
	}
	if span.RemoteEndpoint != nil || len(span.Tags) != 0 {
		return fmt.Errorf("v2 phase span %q contains unexpected endpoint or tags", span.Name)
	}
	return validateDifferentialZipkinAnnotations(span.Annotations)
}

func validateDifferentialZipkinTiming(span differentialZipkinSpan) error {
	if !validDifferentialZipkinNumber(span.Timestamp, false) {
		return fmt.Errorf("span %q timestamp = %q, want a positive number", span.Name, span.Timestamp)
	}
	if !validDifferentialZipkinNumber(span.Duration, true) {
		return fmt.Errorf("span %q duration = %q, want a nonnegative number", span.Name, span.Duration)
	}
	return nil
}

func validDifferentialZipkinNumber(number json.Number, allowZero bool) bool {
	value, err := number.Float64()
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return false
	}
	return value > 0 || allowZero && value == 0
}

func validateDifferentialZipkinLocalEndpoint(endpoint *differentialZipkinEndpoint) error {
	if endpoint == nil || endpoint.ServiceName != "APISIX" {
		return fmt.Errorf("localEndpoint serviceName = %q, want APISIX", differentialZipkinServiceName(endpoint))
	}
	if endpoint.IPv4 != "127.0.0.1" || endpoint.IPv6 != "" || endpoint.Port < 1 || endpoint.Port > 65535 {
		return fmt.Errorf("localEndpoint address is not the configured numeric loopback endpoint")
	}
	return nil
}

func differentialZipkinServiceName(endpoint *differentialZipkinEndpoint) string {
	if endpoint == nil {
		return ""
	}
	return endpoint.ServiceName
}

func validateDifferentialZipkinRemoteEndpoint(endpoint *differentialZipkinEndpoint) error {
	if endpoint == nil || endpoint.ServiceName != "" || endpoint.Port < 1 || endpoint.Port > 65535 {
		return fmt.Errorf("remoteEndpoint is missing a valid dynamic address")
	}
	if endpoint.IPv4 != "" {
		if ip := net.ParseIP(endpoint.IPv4); ip == nil || ip.To4() == nil || endpoint.IPv6 != "" {
			return fmt.Errorf("remoteEndpoint ipv4 = %q is invalid", endpoint.IPv4)
		}
		return nil
	}
	if ip := net.ParseIP(endpoint.IPv6); ip == nil || ip.To4() != nil {
		return fmt.Errorf("remoteEndpoint ipv6 = %q is invalid", endpoint.IPv6)
	}
	return nil
}

func validateDifferentialZipkinServerTags(tags map[string]string) error {
	want := map[string]string{
		"component":              "apisix",
		"http.method":            http.MethodGet,
		"http.url":               "/zipkin/v2",
		"http.status_code":       "200",
		"apisix.response_source": "upstream",
	}
	for name, value := range tags {
		wanted, exists := want[name]
		if !exists {
			return fmt.Errorf("unknown tag %q", name)
		}
		if value != wanted {
			return fmt.Errorf("tag %q = %q, want %q", name, value, wanted)
		}
	}
	for name := range want {
		if _, exists := tags[name]; !exists {
			return fmt.Errorf("missing tag %q", name)
		}
	}
	return nil
}

func validateDifferentialZipkinAnnotations(raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "[]" {
		return nil
	}
	return fmt.Errorf("span annotations are outside the pinned SERVER-span contract")
}

func validateDifferentialZipkinOriginHeaders(
	headers map[string][]string,
	wantSpanID string,
	wantParentID string,
) error {
	if len(headers) != 5 {
		return fmt.Errorf("semantic header count = %d, want 5", len(headers))
	}
	want := map[string]string{
		"X-B3-TraceId":      differentialZipkinTraceID,
		"X-B3-SpanId":       wantSpanID,
		"X-B3-ParentSpanId": wantParentID,
		"X-B3-Sampled":      "1",
		"X-B3-Flags":        "1",
	}
	for name, value := range want {
		got, err := singleDifferentialHeader(headers, name)
		if err != nil || got != value {
			label := strings.TrimPrefix(strings.ToLower(name), "x-b3-")
			return fmt.Errorf("%s = %q, want %q: %v", label, got, value, err)
		}
	}
	for name := range headers {
		if _, exists := want[http.CanonicalHeaderKey(name)]; !exists {
			matched := false
			for expected := range want {
				matched = matched || strings.EqualFold(name, expected)
			}
			if !matched {
				return fmt.Errorf("unapproved semantic header %q", name)
			}
		}
	}
	return nil
}

func validDifferentialZipkinID(value string, bytes int) bool {
	if len(value) != bytes*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes
}

func differentialZipkinCanonicalServerSpan(span differentialZipkinSpan) string {
	return `[{"traceId":"` + span.TraceID + `","name":"apisix.request",` +
		`"parentId":"` + differentialZipkinIncomingSpanID + `","id":"<server-span-id>",` +
		`"kind":"SERVER","timestamp":"<dynamic>","duration":"<dynamic>",` +
		`"localEndpoint":{"serviceName":"APISIX","address":"<dynamic>"},` +
		`"remoteEndpoint":{"address":"<dynamic>"},` +
		`"tags":{"component":"apisix","http.method":"GET","http.url":"/zipkin/v2",` +
		`"http.status_code":"200"}}]`
}
