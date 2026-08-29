package pluginintegration

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	collecttracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type differentialOpenTelemetryCore struct {
	ServiceName    string `json:"service_name"`
	SpanName       string `json:"span_name"`
	SpanKind       string `json:"span_kind"`
	TraceID        string `json:"trace_id"`
	RouteID        string `json:"route_id"`
	RouteName      string `json:"route_name"`
	Route          string `json:"route"`
	Method         string `json:"method"`
	Target         string `json:"target"`
	Host           string `json:"host"`
	Status         int64  `json:"status"`
	ResponseSource string `json:"response_source"`
	TenantArgument string `json:"tenant_argument"`
	TenantHeader   string `json:"tenant_header"`
}

func compareDifferentialOpenTelemetryOTLPHTTPServerSpanCore(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialOpenTelemetryCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned OpenTelemetry case",
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
		if err := normalizeDifferentialOpenTelemetryObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialOpenTelemetryObservation(
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

	origin, collector, err := differentialOpenTelemetryCalls(spec, side, observation.UpstreamCalls)
	if err != nil {
		return err
	}
	if origin.Host != "differential.example.test" || origin.Body != "" {
		return fmt.Errorf("comparison policy %q %s origin request is not the pinned GET", spec.ComparisonPolicy, side)
	}
	if err := validateDifferentialLoggerHeaders(origin.Headers, map[string]string{
		"X-Request-Id": differentialOpenTelemetryTraceID,
		"X-Tenant":     "blue",
	}); err != nil {
		return fmt.Errorf("comparison policy %q %s origin headers: %w", spec.ComparisonPolicy, side, err)
	}
	if err := validateDifferentialLoggerFixtureHost(
		side, collector.Host, observation.UpstreamAddress, false,
	); err != nil {
		return fmt.Errorf("comparison policy %q %s collector Host: %w", spec.ComparisonPolicy, side, err)
	}
	if err := validateDifferentialLoggerHeaders(collector.Headers, map[string]string{
		"Content-Type":        "application/x-protobuf",
		"X-Differential-OTel": "contract-v1",
	}); err != nil {
		return fmt.Errorf("comparison policy %q %s collector headers: %w", spec.ComparisonPolicy, side, err)
	}
	core, err := validateDifferentialOpenTelemetryPayload(side, []byte(collector.Body))
	if err != nil {
		return fmt.Errorf("comparison policy %q %s collector protobuf: %w", spec.ComparisonPolicy, side, err)
	}
	canonical, err := json.Marshal(core)
	if err != nil {
		return fmt.Errorf("marshal canonical OpenTelemetry core: %w", err)
	}

	origin.Path = wantStep.Request.Path
	collector.Path = differentialOpenTelemetryCollectorPath
	collector.Host = "fixture:" + spec.Fixture.Name
	collector.Body = string(canonical)
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	observation.UpstreamCalls = []DifferentialUpstreamObservation{origin, collector}
	observation.Upstream = collector
	return nil
}

func differentialOpenTelemetryCalls(
	spec DifferentialCase,
	side string,
	calls []DifferentialUpstreamObservation,
) (DifferentialUpstreamObservation, DifferentialUpstreamObservation, error) {
	var origin DifferentialUpstreamObservation
	var collector DifferentialUpstreamObservation
	for _, call := range calls {
		if !call.Received || call.Fixture != spec.Fixture.Name {
			return origin, collector, fmt.Errorf(
				"comparison policy %q %s fixture call identity is invalid", spec.ComparisonPolicy, side,
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
			differentialLoggerRequestTargetMatches(call.Path, differentialOpenTelemetryCollectorPath):
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
			spec.ComparisonPolicy, side, spec.Steps[0].Request.Path,
		)
	}
	if !collector.Received {
		return origin, collector, fmt.Errorf(
			"comparison policy %q %s fixture calls are missing POST %s",
			spec.ComparisonPolicy, side, differentialOpenTelemetryCollectorPath,
		)
	}
	return origin, collector, nil
}

func validateDifferentialOpenTelemetryPayload(
	side string,
	body []byte,
) (differentialOpenTelemetryCore, error) {
	request := &collecttracepb.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(body, request); err != nil {
		return differentialOpenTelemetryCore{}, fmt.Errorf("decode OTLP protobuf: %w", err)
	}
	if len(request.ResourceSpans) != 1 {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"resource_spans = %d, want exactly one",
			len(request.ResourceSpans),
		)
	}
	resourceSpans := request.ResourceSpans[0]
	if resourceSpans == nil || resourceSpans.Resource == nil || resourceSpans.SchemaUrl != "" {
		return differentialOpenTelemetryCore{}, fmt.Errorf("resource span envelope is not the pinned shape")
	}
	resourceAttributes, err := differentialOpenTelemetryAttributeMap(resourceSpans.Resource.Attributes)
	if err != nil {
		return differentialOpenTelemetryCore{}, fmt.Errorf("resource attributes: %w", err)
	}
	if len(resourceAttributes) != 5 {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"resource attribute count = %d, want 5",
			len(resourceAttributes),
		)
	}
	serviceName, err := differentialOpenTelemetryRequiredString(
		resourceAttributes, "service.name", differentialOpenTelemetryServiceName,
	)
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	hostname, err := differentialOpenTelemetryString(resourceAttributes["hostname"])
	if err != nil || strings.TrimSpace(hostname) == "" {
		return differentialOpenTelemetryCore{}, fmt.Errorf("hostname is not a nonempty string")
	}
	for name, want := range map[string]string{
		"telemetry.sdk.language": "lua",
		"telemetry.sdk.name":     "opentelemetry-lua",
		"telemetry.sdk.version":  "0.1.1",
	} {
		if _, err := differentialOpenTelemetryRequiredString(resourceAttributes, name, want); err != nil {
			return differentialOpenTelemetryCore{}, err
		}
	}
	if len(resourceSpans.ScopeSpans) != 1 {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"scope_spans = %d, want exactly one",
			len(resourceSpans.ScopeSpans),
		)
	}
	scopeSpans := resourceSpans.ScopeSpans[0]
	if scopeSpans == nil || scopeSpans.Scope == nil || scopeSpans.Scope.Name != "opentelemetry-lua" ||
		scopeSpans.Scope.Version != "" || len(scopeSpans.Scope.Attributes) != 0 || scopeSpans.SchemaUrl != "" {
		return differentialOpenTelemetryCore{}, fmt.Errorf("instrumentation scope is not the pinned %s identity", side)
	}
	if len(scopeSpans.Spans) != 1 {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"scope contains %d spans, want exactly one span",
			len(scopeSpans.Spans),
		)
	}
	span := scopeSpans.Spans[0]
	if span == nil || span.Name != "GET /otel/trace" {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"span name = %q, want GET /otel/trace",
			differentialOpenTelemetrySpanName(span),
		)
	}
	if span.Kind != tracepb.Span_SPAN_KIND_SERVER {
		return differentialOpenTelemetryCore{}, fmt.Errorf("span kind = %s, want SERVER", span.Kind)
	}
	wantTraceID, _ := hex.DecodeString(differentialOpenTelemetryTraceID)
	if !bytes.Equal(span.TraceId, wantTraceID) {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"trace_id = %x, want %s",
			span.TraceId,
			differentialOpenTelemetryTraceID,
		)
	}
	if len(span.SpanId) != 8 || allDifferentialOpenTelemetryZero(span.SpanId) {
		return differentialOpenTelemetryCore{}, fmt.Errorf("span_id = %x, want a nonzero 8-byte ID", span.SpanId)
	}
	if len(span.ParentSpanId) != 0 {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"parent_span_id = %x, want empty root parent",
			span.ParentSpanId,
		)
	}
	if span.StartTimeUnixNano == 0 || span.EndTimeUnixNano < span.StartTimeUnixNano {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"span timing = %d..%d, want a positive ordered interval",
			span.StartTimeUnixNano, span.EndTimeUnixNano,
		)
	}
	if len(span.Events) != 0 || len(span.Links) != 0 || span.DroppedAttributesCount != 0 ||
		span.DroppedEventsCount != 0 || span.DroppedLinksCount != 0 ||
		(span.Status != nil && span.Status.Code != tracepb.Status_STATUS_CODE_UNSET) {
		return differentialOpenTelemetryCore{}, fmt.Errorf("span contains unapproved events, links, drops, or status")
	}

	attributes, err := differentialOpenTelemetryAttributeMap(span.Attributes)
	if err != nil {
		return differentialOpenTelemetryCore{}, fmt.Errorf("span attributes: %w", err)
	}
	allowed := differentialOpenTelemetryAllowedSpanAttributes()
	for name := range attributes {
		if _, ok := allowed[name]; !ok {
			return differentialOpenTelemetryCore{}, fmt.Errorf("unknown attribute %q", name)
		}
	}
	routeID, err := differentialOpenTelemetryRequiredString(
		attributes,
		"apisix.route_id",
		differentialOpenTelemetryRouteID,
	)
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	routeName, err := differentialOpenTelemetryRequiredString(
		attributes,
		"apisix.route_name",
		differentialOpenTelemetryRouteName,
	)
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	route, err := differentialOpenTelemetryRequiredString(attributes, "http.route", "/otel/trace")
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	method, err := differentialOpenTelemetryRequiredString(attributes, "http.method", http.MethodGet)
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	target, err := differentialOpenTelemetryRequiredString(attributes, "http.target", "/otel/trace?tenant=blue")
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	host, err := differentialOpenTelemetryRequiredString(attributes, "net.host.name", "gateway.example.test")
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	status, err := differentialOpenTelemetryRequiredInt(attributes, "http.status_code", http.StatusOK)
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	responseSource, err := differentialOpenTelemetryRequiredString(attributes, "apisix.response_source", "upstream")
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	tenantArgument, err := differentialOpenTelemetryRequiredString(attributes, "arg_tenant", "blue")
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	tenantHeader, err := differentialOpenTelemetryRequiredString(attributes, "x-tenant", "blue")
	if err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	if err := validateDifferentialOpenTelemetrySemanticAttributes(attributes); err != nil {
		return differentialOpenTelemetryCore{}, err
	}
	if len(attributes) != len(allowed) {
		return differentialOpenTelemetryCore{}, fmt.Errorf(
			"span attribute count = %d, want %d", len(attributes), len(allowed),
		)
	}

	return differentialOpenTelemetryCore{
		ServiceName: serviceName, SpanName: span.Name, SpanKind: "SERVER",
		TraceID: differentialOpenTelemetryTraceID, RouteID: routeID, RouteName: routeName,
		Route: route, Method: method, Target: target, Host: host, Status: status,
		ResponseSource: responseSource, TenantArgument: tenantArgument, TenantHeader: tenantHeader,
	}, nil
}

func differentialOpenTelemetrySpanName(span *tracepb.Span) string {
	if span == nil {
		return ""
	}
	return span.Name
}

func differentialOpenTelemetryAllowedSpanAttributes() map[string]struct{} {
	return map[string]struct{}{
		"apisix.route_id": {}, "apisix.route_name": {}, "http.route": {},
		"http.method": {}, "http.target": {}, "net.host.name": {},
		"http.status_code": {}, "apisix.response_source": {}, "arg_tenant": {}, "x-tenant": {},
		"http.user_agent": {}, "http.scheme": {}, "http.request.method": {},
		"url.scheme": {}, "url.path": {}, "user_agent.original": {},
		"http.response.status_code": {},
	}
}

func validateDifferentialOpenTelemetrySemanticAttributes(attributes map[string]*commonpb.AnyValue) error {
	stringValues := map[string]string{
		"http.user_agent": "Go-http-client/1.1", "http.scheme": "http",
		"http.request.method": http.MethodGet, "url.scheme": "http", "url.path": "/otel/trace",
		"user_agent.original": "Go-http-client/1.1",
	}
	for name, want := range stringValues {
		if _, err := differentialOpenTelemetryRequiredString(attributes, name, want); err != nil {
			return err
		}
	}
	if _, err := differentialOpenTelemetryRequiredInt(
		attributes, "http.response.status_code", http.StatusOK,
	); err != nil {
		return err
	}
	return nil
}

func differentialOpenTelemetryAttributeMap(
	attributes []*commonpb.KeyValue,
) (map[string]*commonpb.AnyValue, error) {
	result := make(map[string]*commonpb.AnyValue, len(attributes))
	for _, attribute := range attributes {
		if attribute == nil || strings.TrimSpace(attribute.Key) == "" || attribute.Value == nil {
			return nil, fmt.Errorf("contains an incomplete attribute")
		}
		if _, duplicate := result[attribute.Key]; duplicate {
			return nil, fmt.Errorf("contains duplicate attribute %q", attribute.Key)
		}
		result[attribute.Key] = attribute.Value
	}
	return result, nil
}

func differentialOpenTelemetryRequiredString(
	attributes map[string]*commonpb.AnyValue,
	name string,
	want string,
) (string, error) {
	value, exists := attributes[name]
	if !exists {
		return "", fmt.Errorf("missing attribute %q", name)
	}
	got, err := differentialOpenTelemetryString(value)
	if err != nil || got != want {
		return "", fmt.Errorf("attribute %q = %q, want %q", name, got, want)
	}
	return got, nil
}

func differentialOpenTelemetryRequiredInt(
	attributes map[string]*commonpb.AnyValue,
	name string,
	want int64,
) (int64, error) {
	value, exists := attributes[name]
	if !exists {
		return 0, fmt.Errorf("missing attribute %q", name)
	}
	typed, ok := value.Value.(*commonpb.AnyValue_IntValue)
	if !ok || typed.IntValue != want {
		return 0, fmt.Errorf("attribute %q is not integer %d", name, want)
	}
	return typed.IntValue, nil
}

func differentialOpenTelemetryString(value *commonpb.AnyValue) (string, error) {
	if value == nil {
		return "", fmt.Errorf("value is nil")
	}
	typed, ok := value.Value.(*commonpb.AnyValue_StringValue)
	if !ok {
		return "", fmt.Errorf("value is not a string")
	}
	return typed.StringValue, nil
}

func allDifferentialOpenTelemetryZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
