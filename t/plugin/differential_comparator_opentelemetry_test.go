package pluginintegration

import (
	"encoding/hex"
	"net/http"
	"reflect"
	"strings"
	"testing"

	collecttracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestCompareDifferentialOpenTelemetryOTLPHTTPServerSpanAcceptsOnlyPinnedCoreAndDocumentedDynamics(
	t *testing.T,
) {
	spec := differentialOpenTelemetryCases()[0]
	candidate, oracle := differentialOpenTelemetryComparatorObservations(t, spec)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialOpenTelemetryOTLPHTTPServerSpanCore(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned OpenTelemetry observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("OpenTelemetry comparison mutated caller observations")
	}
}

func TestCompareDifferentialOpenTelemetryOTLPHTTPServerSpanRejectsLooseContracts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned case",
			mutate: func(_ *testing.T, spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Fixture.ExpectedCalls++
			},
			want: "pinned",
		},
		{
			name: "collector path",
			mutate: func(_ *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialOpenTelemetryTestCall(candidate, http.MethodPost, differentialOpenTelemetryCollectorPath)
				call.Path = "/wrong"
				candidate.Upstream = *call
			},
			want: "missing POST",
		},
		{
			name: "collector content type",
			mutate: func(_ *testing.T, _ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialOpenTelemetryTestCall(oracle, http.MethodPost, differentialOpenTelemetryCollectorPath).
					Headers["Content-Type"] = []string{"application/json"}
			},
			want: "Content-Type",
		},
		{
			name: "unexpected compression",
			mutate: func(_ *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				differentialOpenTelemetryTestCall(candidate, http.MethodPost, differentialOpenTelemetryCollectorPath).
					Headers["Content-Encoding"] = []string{"gzip"}
			},
			want: "unapproved semantic header",
		},
		{
			name: "malformed protobuf",
			mutate: func(_ *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := differentialOpenTelemetryTestCall(candidate, http.MethodPost, differentialOpenTelemetryCollectorPath)
				call.Body = "not-otlp"
				candidate.Upstream = *call
			},
			want: "protobuf",
		},
		{
			name: "service name",
			mutate: func(t *testing.T, _ *DifferentialCase, _, oracle *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, oracle, func(request *collecttracepb.ExportTraceServiceRequest) {
					request.ResourceSpans[0].Resource.Attributes[0].Value = differentialOTLPStringValue("other")
				})
			},
			want: "service.name",
		},
		{
			name: "SDK language",
			mutate: func(t *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, candidate, func(request *collecttracepb.ExportTraceServiceRequest) {
					request.ResourceSpans[0].Resource.Attributes[2].Value = differentialOTLPStringValue("go")
				})
			},
			want: "telemetry.sdk.language",
		},
		{
			name: "instrumentation scope",
			mutate: func(t *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, candidate, func(request *collecttracepb.ExportTraceServiceRequest) {
					request.ResourceSpans[0].ScopeSpans[0].Scope.Name = "opentelemetry"
				})
			},
			want: "instrumentation scope",
		},
		{
			name: "route id",
			mutate: func(t *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, candidate, func(request *collecttracepb.ExportTraceServiceRequest) {
					setDifferentialOTLPAttribute(request.ResourceSpans[0].ScopeSpans[0].Spans[0], "apisix.route_id", differentialOTLPStringValue("other"))
				})
			},
			want: "apisix.route_id",
		},
		{
			name: "span name",
			mutate: func(t *testing.T, _ *DifferentialCase, _, oracle *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, oracle, func(request *collecttracepb.ExportTraceServiceRequest) {
					request.ResourceSpans[0].ScopeSpans[0].Spans[0].Name = "GET /wrong"
				})
			},
			want: "span name",
		},
		{
			name: "span kind",
			mutate: func(t *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, candidate, func(request *collecttracepb.ExportTraceServiceRequest) {
					request.ResourceSpans[0].ScopeSpans[0].Spans[0].Kind = tracepb.Span_SPAN_KIND_CLIENT
				})
			},
			want: "span kind",
		},
		{
			name: "trace id",
			mutate: func(t *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, candidate, func(request *collecttracepb.ExportTraceServiceRequest) {
					request.ResourceSpans[0].ScopeSpans[0].Spans[0].TraceId = []byte(strings.Repeat("x", 16))
				})
			},
			want: "trace_id",
		},
		{
			name: "zero span id",
			mutate: func(t *testing.T, _ *DifferentialCase, _, oracle *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, oracle, func(request *collecttracepb.ExportTraceServiceRequest) {
					request.ResourceSpans[0].ScopeSpans[0].Spans[0].SpanId = make([]byte, 8)
				})
			},
			want: "span_id",
		},
		{
			name: "second span",
			mutate: func(t *testing.T, _ *DifferentialCase, _, oracle *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, oracle, func(request *collecttracepb.ExportTraceServiceRequest) {
					scope := request.ResourceSpans[0].ScopeSpans[0]
					scope.Spans = append(scope.Spans, proto.Clone(scope.Spans[0]).(*tracepb.Span))
				})
			},
			want: "exactly one span",
		},
		{
			name: "unknown span attribute",
			mutate: func(t *testing.T, _ *DifferentialCase, candidate, _ *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, candidate, func(request *collecttracepb.ExportTraceServiceRequest) {
					span := request.ResourceSpans[0].ScopeSpans[0].Spans[0]
					span.Attributes = append(span.Attributes, differentialOTLPKeyValue("unknown", differentialOTLPStringValue("value")))
				})
			},
			want: "unknown attribute",
		},
		{
			name: "missing APISIX semantic attribute",
			mutate: func(t *testing.T, _ *DifferentialCase, _, oracle *DifferentialObservation) {
				mutateDifferentialOpenTelemetryPayload(t, oracle, func(request *collecttracepb.ExportTraceServiceRequest) {
					span := request.ResourceSpans[0].ScopeSpans[0].Spans[0]
					removeDifferentialOTLPAttribute(span, "http.response.status_code")
				})
			},
			want: "http.response.status_code",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialOpenTelemetryCases()[0]
			candidate, oracle := differentialOpenTelemetryComparatorObservations(t, spec)
			test.mutate(t, &spec, &candidate, &oracle)

			passed, _, err := compareDifferentialOpenTelemetryOTLPHTTPServerSpanCore(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose OpenTelemetry contract = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialOpenTelemetryComparatorObservations(
	t *testing.T,
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	t.Helper()
	candidateOrigin := differentialOpenTelemetryOriginCall(spec)
	candidateCollector := differentialOpenTelemetryCollectorCall(
		t,
		spec,
		"127.0.0.1:31131",
		"opentelemetry-lua",
		"candidate-host",
		1700000000000000000,
		1700000000001000000,
		"aaaaaaaaaaaaaaaa",
	)
	candidate := differentialOpenTelemetryObservation(
		spec, "127.0.0.1:31131", candidateOrigin, candidateCollector,
	)

	oracleOrigin := differentialOpenTelemetryOriginCall(spec)
	oracleCollector := differentialOpenTelemetryCollectorCall(
		t,
		spec,
		"host.containers.internal:31131",
		"opentelemetry-lua",
		"oracle-host",
		1700000001000000000,
		1700000001002000000,
		"bbbbbbbbbbbbbbbb",
	)
	oracle := differentialOpenTelemetryObservation(
		spec, "host.containers.internal:31131", oracleCollector, oracleOrigin,
	)
	return candidate, oracle
}

func differentialOpenTelemetryObservation(
	spec DifferentialCase,
	address string,
	calls ...DifferentialUpstreamObservation,
) DifferentialObservation {
	return DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status: spec.Fixture.Response.Status, Body: spec.Fixture.Response.Body,
			Host: spec.Steps[0].Request.Host, SecurityDecision: spec.Steps[0].SecurityDecision,
		}},
		UpstreamFixture: spec.Fixture.Name,
		UpstreamAddress: address,
		UpstreamCalls:   calls,
		Upstream:        calls[len(calls)-1],
	}
}

func differentialOpenTelemetryOriginCall(spec DifferentialCase) DifferentialUpstreamObservation {
	return DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodGet,
		Path: spec.Steps[0].Request.Path, Host: "differential.example.test",
		Headers: map[string][]string{
			"X-Request-Id": {differentialOpenTelemetryTraceID},
			"X-Tenant":     {"blue"},
		},
	}
}

func differentialOpenTelemetryCollectorCall(
	t *testing.T,
	spec DifferentialCase,
	host string,
	scopeName string,
	hostname string,
	started uint64,
	ended uint64,
	spanID string,
) DifferentialUpstreamObservation {
	t.Helper()
	traceID, err := hex.DecodeString(differentialOpenTelemetryTraceID)
	if err != nil {
		t.Fatalf("decode trace ID: %v", err)
	}
	decodedSpanID, err := hex.DecodeString(spanID)
	if err != nil {
		t.Fatalf("decode span ID: %v", err)
	}
	request := &collecttracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			differentialOTLPKeyValue("service.name", differentialOTLPStringValue(differentialOpenTelemetryServiceName)),
			differentialOTLPKeyValue("hostname", differentialOTLPStringValue(hostname)),
			differentialOTLPKeyValue("telemetry.sdk.language", differentialOTLPStringValue("lua")),
			differentialOTLPKeyValue("telemetry.sdk.name", differentialOTLPStringValue("opentelemetry-lua")),
			differentialOTLPKeyValue("telemetry.sdk.version", differentialOTLPStringValue("0.1.1")),
		}},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: scopeName},
			Spans: []*tracepb.Span{{
				TraceId: traceID, SpanId: decodedSpanID,
				Name: "GET /otel/trace", Kind: tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: started, EndTimeUnixNano: ended,
				Attributes: differentialOpenTelemetryCoreAttributes(),
			}},
		}},
	}}}
	body, err := proto.Marshal(request)
	if err != nil {
		t.Fatalf("marshal OTLP request: %v", err)
	}
	return DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodPost,
		Path: differentialOpenTelemetryCollectorPath, Host: host,
		Headers: map[string][]string{
			"Content-Type":        {"application/x-protobuf"},
			"X-Differential-OTel": {"contract-v1"},
		},
		Body: string(body),
	}
}

func differentialOpenTelemetryCoreAttributes() []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		differentialOTLPKeyValue("apisix.route_id", differentialOTLPStringValue(differentialOpenTelemetryRouteID)),
		differentialOTLPKeyValue("apisix.route_name", differentialOTLPStringValue(differentialOpenTelemetryRouteName)),
		differentialOTLPKeyValue("http.route", differentialOTLPStringValue("/otel/trace")),
		differentialOTLPKeyValue("http.method", differentialOTLPStringValue(http.MethodGet)),
		differentialOTLPKeyValue("http.scheme", differentialOTLPStringValue("http")),
		differentialOTLPKeyValue("http.target", differentialOTLPStringValue("/otel/trace?tenant=blue")),
		differentialOTLPKeyValue("http.user_agent", differentialOTLPStringValue("Go-http-client/1.1")),
		differentialOTLPKeyValue("http.request.method", differentialOTLPStringValue(http.MethodGet)),
		differentialOTLPKeyValue("net.host.name", differentialOTLPStringValue("gateway.example.test")),
		differentialOTLPKeyValue("url.path", differentialOTLPStringValue("/otel/trace")),
		differentialOTLPKeyValue("url.scheme", differentialOTLPStringValue("http")),
		differentialOTLPKeyValue("user_agent.original", differentialOTLPStringValue("Go-http-client/1.1")),
		differentialOTLPKeyValue("http.status_code", differentialOTLPIntValue(http.StatusOK)),
		differentialOTLPKeyValue("http.response.status_code", differentialOTLPIntValue(http.StatusOK)),
		differentialOTLPKeyValue("apisix.response_source", differentialOTLPStringValue("upstream")),
		differentialOTLPKeyValue("arg_tenant", differentialOTLPStringValue("blue")),
		differentialOTLPKeyValue("x-tenant", differentialOTLPStringValue("blue")),
	}
}

func differentialOTLPKeyValue(key string, value *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: value}
}

func differentialOTLPStringValue(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}

func differentialOTLPIntValue(value int) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(value)}}
}

func setDifferentialOTLPAttribute(span *tracepb.Span, name string, value *commonpb.AnyValue) {
	for _, attribute := range span.Attributes {
		if attribute.Key == name {
			attribute.Value = value
			return
		}
	}
	panic("missing OTLP attribute " + name)
}

func removeDifferentialOTLPAttribute(span *tracepb.Span, name string) {
	for index, attribute := range span.Attributes {
		if attribute.Key == name {
			span.Attributes = append(span.Attributes[:index], span.Attributes[index+1:]...)
			return
		}
	}
	panic("missing OTLP attribute " + name)
}

func mutateDifferentialOpenTelemetryPayload(
	t *testing.T,
	observation *DifferentialObservation,
	mutate func(*collecttracepb.ExportTraceServiceRequest),
) {
	t.Helper()
	call := differentialOpenTelemetryTestCall(observation, http.MethodPost, differentialOpenTelemetryCollectorPath)
	request := &collecttracepb.ExportTraceServiceRequest{}
	if err := proto.Unmarshal([]byte(call.Body), request); err != nil {
		t.Fatalf("unmarshal OTLP request: %v", err)
	}
	mutate(request)
	body, err := proto.Marshal(request)
	if err != nil {
		t.Fatalf("marshal mutated OTLP request: %v", err)
	}
	call.Body = string(body)
	if observation.Upstream.Method == http.MethodPost {
		observation.Upstream = *call
	}
}

func differentialOpenTelemetryTestCall(
	observation *DifferentialObservation,
	method string,
	path string,
) *DifferentialUpstreamObservation {
	for index := range observation.UpstreamCalls {
		call := &observation.UpstreamCalls[index]
		if call.Method == method && call.Path == path {
			return call
		}
	}
	panic("missing OpenTelemetry test call " + method + " " + path)
}
