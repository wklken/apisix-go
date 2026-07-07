package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestPostInitSetsSamplerDefaults(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	if p.config.Sampler.Name != "always_off" {
		t.Fatalf("sampler name = %q, want always_off", p.config.Sampler.Name)
	}
	if p.config.Sampler.Options.Fraction != 0 {
		t.Fatalf("sampler fraction = %v, want 0", p.config.Sampler.Options.Fraction)
	}
	if p.serverName() != "APISIX" {
		t.Fatalf("server name = %q, want APISIX", p.serverName())
	}
}

func TestAdditionalSpanAttributesUseRequestVarsAndHeaders(t *testing.T) {
	p := &Plugin{
		config: Config{
			AdditionalAttributes:             []string{"request_method", "uri", "missing_var"},
			AdditionalHeaderPrefixAttributes: []string{"x-tenant", "x-extra-*", "x-missing"},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/orders?debug=1", nil)
	req.Header.Set("X-Tenant", "blue")
	req.Header.Set("X-Extra-A", "1")
	req.Header.Set("X-Extra-B", "2")

	attrs := p.additionalSpanAttributes(req)
	got := map[string]string{}
	for _, attr := range attrs {
		got[string(attr.Key)] = attr.Value.AsString()
	}

	want := map[string]string{
		"request_method": "POST",
		"uri":            "/orders",
		"x-tenant":       "blue",
		"x-extra-a":      "1",
		"x-extra-b":      "2",
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("attribute %q = %q, want %q; attrs=%v", key, got[key], value, got)
		}
	}
	for _, key := range []string{"missing_var", "x-missing"} {
		if _, ok := got[key]; ok {
			t.Fatalf("attribute %q present, want skipped; attrs=%v", key, got)
		}
	}
}

func TestAdditionalSpanAttributesUseAPISIXAndRequestVars(t *testing.T) {
	p := &Plugin{
		config: Config{
			AdditionalAttributes: []string{"route_id", "service_name", "upstream_latency"},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":     "route-1",
		"$service_name": "orders-service",
	})
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$upstream_latency", int64(37))

	attrs := p.additionalSpanAttributes(req)
	got := map[string]string{}
	for _, attr := range attrs {
		got[string(attr.Key)] = attr.Value.AsString()
	}

	want := map[string]string{
		"route_id":         "route-1",
		"service_name":     "orders-service",
		"upstream_latency": "37",
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("attribute %q = %q, want %q; attrs=%v", key, got[key], value, got)
		}
	}
}

func TestBuildSamplerUsesOfficialSamplerNames(t *testing.T) {
	tests := []struct {
		name    string
		sampler SamplerConfig
		want    sdktrace.SamplingDecision
	}{
		{
			name:    "always off",
			sampler: SamplerConfig{Name: "always_off"},
			want:    sdktrace.Drop,
		},
		{
			name:    "always on",
			sampler: SamplerConfig{Name: "always_on"},
			want:    sdktrace.RecordAndSample,
		},
		{
			name: "trace ratio zero",
			sampler: SamplerConfig{
				Name:    "trace_id_ratio",
				Options: SamplerOptions{Fraction: 0},
			},
			want: sdktrace.Drop,
		},
		{
			name: "trace ratio one",
			sampler: SamplerConfig{
				Name:    "trace_id_ratio",
				Options: SamplerOptions{Fraction: 1},
			},
			want: sdktrace.RecordAndSample,
		},
		{
			name: "parent base uses configured root",
			sampler: SamplerConfig{
				Name: "parent_base",
				Options: SamplerOptions{
					Root: RootSamplerConfig{Name: "always_on"},
				},
			},
			want: sdktrace.RecordAndSample,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSampler(tt.sampler).ShouldSample(sdktrace.SamplingParameters{
				ParentContext: context.Background(),
				Name:          "GET",
			}).Decision
			if got != tt.want {
				t.Fatalf("sampling decision = %v, want %v", got, tt.want)
			}
		})
	}
}
