package otel

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func buildSampler(conf SamplerConfig) sdktrace.Sampler {
	switch conf.Name {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "trace_id_ratio":
		return sdktrace.TraceIDRatioBased(conf.Options.Fraction)
	case "parent_base":
		return sdktrace.ParentBased(buildRootSampler(conf.Options.Root))
	default:
		return sdktrace.NeverSample()
	}
}

func buildRootSampler(conf RootSamplerConfig) sdktrace.Sampler {
	switch conf.Name {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "trace_id_ratio":
		return sdktrace.TraceIDRatioBased(conf.Options.Fraction)
	default:
		return sdktrace.NeverSample()
	}
}

func loadMetadata(
	view runtime.MetadataView,
	pluginAttr map[string]map[string]any,
) (metadata Metadata, configured bool, err error) {
	if found, decodeErr := view.Decode(name, &metadata); found || decodeErr != nil {
		if decodeErr != nil {
			return Metadata{}, false, fmt.Errorf("decode OpenTelemetry metadata: %w", decodeErr)
		}
		return applyMetadataDefaults(metadata), true, nil
	}

	metadata = Metadata{}
	if attr, ok := pluginAttr[name]; ok {
		if attr == nil {
			return Metadata{}, false, fmt.Errorf("OpenTelemetry plugin attributes %q must be an object", name)
		}
		if parseErr := util.Parse(attr, &metadata); parseErr != nil {
			return Metadata{}, false, fmt.Errorf("decode OpenTelemetry plugin attributes: %w", parseErr)
		}
		return applyMetadataDefaults(metadata), true, nil
	}

	return applyMetadataDefaults(metadata), false, nil
}

func applyMetadataDefaults(metadata Metadata) Metadata {
	if metadata.TraceIDSource == "" {
		metadata.TraceIDSource = "random"
	}
	if metadata.Collector.Address == "" {
		metadata.Collector.Address = "127.0.0.1:4318"
	}
	if metadata.Collector.RequestTimeout == 0 {
		metadata.Collector.RequestTimeout = 3
	}
	return metadata
}

func newTracerProviderWithProcessor(
	sampler SamplerConfig,
	metadata Metadata,
	metadataConfigured bool,
	tasks *runtime.TaskOwner,
) (*sdktrace.TracerProvider, *apisixBatchSpanProcessor, error) {
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(buildSampler(sampler)),
		sdktrace.WithResource(otelResource(metadata.Resource)),
	}
	if metadata.TraceIDSource == "x-request-id" {
		options = append(options, sdktrace.WithIDGenerator(requestIDGenerator{}))
	}
	if !metadataConfigured {
		return sdktrace.NewTracerProvider(options...), nil, nil
	}

	exporterOptions := []otlptracehttp.Option{
		otlptracehttp.WithTimeout(time.Duration(metadata.Collector.RequestTimeout) * time.Second),
		otlptracehttp.WithHeaders(stringHeaders(metadata.Collector.RequestHeaders)),
	}
	address := metadata.Collector.Address
	if strings.Contains(address, "://") {
		collectorURL, err := url.Parse(address)
		if err != nil || collectorURL.Host == "" ||
			(collectorURL.Scheme != "http" && collectorURL.Scheme != "https") {
			return nil, nil, fmt.Errorf("invalid OpenTelemetry collector address %q", address)
		}
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpointURL(address))
	} else {
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpoint(address), otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(context.Background(), exporterOptions...)
	if err != nil {
		return nil, nil, fmt.Errorf("create OpenTelemetry OTLP exporter: %w", err)
	}

	processor, err := newAPISIXBatchSpanProcessor(exporter, metadata.BatchSpanProcessor, tasks)
	if err != nil {
		_ = exporter.Shutdown(context.Background())
		return nil, nil, err
	}
	options = append(options, sdktrace.WithSpanProcessor(processor))
	return sdktrace.NewTracerProvider(options...), processor, nil
}

func otelResource(configured map[string]any) *sdkresource.Resource {
	configured = flattenResourceAttributes(configured)
	hostname, _ := os.Hostname()
	attributes := []attribute.KeyValue{
		attribute.String("hostname", hostname),
		attribute.String("telemetry.sdk.language", "lua"),
		attribute.String("telemetry.sdk.name", "opentelemetry-lua"),
		attribute.String("telemetry.sdk.version", "0.1.1"),
	}
	if _, ok := configured["service.name"]; !ok {
		attributes = append(attributes, attribute.String("service.name", "APISIX"))
	}
	for key, value := range configured {
		switch typed := value.(type) {
		case string:
			attributes = append(attributes, attribute.String(key, typed))
		case bool:
			attributes = append(attributes, attribute.Bool(key, typed))
		case float64:
			attributes = append(attributes, attribute.Float64(key, typed))
		case int:
			attributes = append(attributes, attribute.Int(key, typed))
		case json.Number:
			attributes = appendJSONNumberResourceAttribute(attributes, key, typed)
		}
	}
	return sdkresource.NewWithAttributes("", attributes...)
}

func appendJSONNumberResourceAttribute(
	attributes []attribute.KeyValue,
	key string,
	value json.Number,
) []attribute.KeyValue {
	if integer, err := strconv.ParseInt(string(value), 10, 64); err == nil {
		return append(attributes, attribute.Int64(key, integer))
	}
	if fraction, err := strconv.ParseFloat(string(value), 64); err == nil {
		return append(attributes, attribute.Float64(key, fraction))
	}
	return attributes
}

func flattenResourceAttributes(configured map[string]any) map[string]any {
	flattened := make(map[string]any)
	var add func(string, any)
	add = func(key string, value any) {
		switch nested := value.(type) {
		case map[string]any:
			for childKey, childValue := range nested {
				fullKey := childKey
				if key != "" {
					fullKey = key + "." + childKey
				}
				add(fullKey, childValue)
			}
		case map[string]string:
			for childKey, childValue := range nested {
				fullKey := childKey
				if key != "" {
					fullKey = key + "." + childKey
				}
				add(fullKey, childValue)
			}
		default:
			flattened[key] = value
		}
	}
	for key, value := range configured {
		add(key, value)
	}
	return flattened
}

func stringHeaders(headers map[string]any) map[string]string {
	result := make(map[string]string, len(headers))
	for key, value := range headers {
		result[key] = fmt.Sprint(value)
	}
	return result
}
