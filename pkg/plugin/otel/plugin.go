package otel

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/riandyrn/otelchi"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// version  = "0.1"
	priority = 12009
	name     = "opentelemetry"
)

const schema = `
{
  "$schema": "http://json-schema.org/draft-04/schema#",
  "type": "object",
  "properties": {
    "sampler": {
      "type": "object",
      "properties": {
        "name": {
          "type": "string",
          "enum": ["always_on", "always_off", "trace_id_ratio", "parent_base"],
          "default": "always_off"
        },
        "options": {
          "type": "object",
          "properties": {
            "fraction": {
              "type": "number",
              "default": 0
            },
            "root": {
              "type": "object",
              "properties": {
                "name": {
                  "type": "string",
                  "enum": ["always_on", "always_off", "trace_id_ratio"],
                  "default": "always_off"
                },
                "options": {
                  "type": "object",
                  "properties": {
                    "fraction": {
                      "type": "number",
                      "default": 0
                    }
                  }
                }
              }
            }
          }
        }
      }
    },
    "additional_attributes": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1
      }
    },
    "additional_header_prefix_attributes": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1
      }
    },
    "server_name": {
      "type": "string"
    }
  }
}
`

type Plugin struct {
	base.BasePlugin
	config Config
}

type Config struct {
	Sampler                          SamplerConfig `json:"sampler,omitempty"`
	AdditionalAttributes             []string      `json:"additional_attributes,omitempty"`
	AdditionalHeaderPrefixAttributes []string      `json:"additional_header_prefix_attributes,omitempty"`
	ServerName                       string        `json:"server_name,omitempty"`
}

type SamplerConfig struct {
	Name    string         `json:"name,omitempty"`
	Options SamplerOptions `json:"options,omitempty"`
}

type SamplerOptions struct {
	Fraction float64           `json:"fraction,omitempty"`
	Root     RootSamplerConfig `json:"root,omitempty"`
}

type RootSamplerConfig struct {
	Name    string             `json:"name,omitempty"`
	Options RootSamplerOptions `json:"options,omitempty"`
}

type RootSamplerOptions struct {
	Fraction float64 `json:"fraction,omitempty"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Sampler.Name == "" {
		p.config.Sampler.Name = "always_off"
	}
	if p.config.Sampler.Options.Root.Name == "" {
		p.config.Sampler.Options.Root.Name = "always_off"
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	wrappedNext := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attrs := p.additionalSpanAttributes(r); len(attrs) > 0 {
			trace.SpanFromContext(r.Context()).SetAttributes(attrs...)
		}
		next.ServeHTTP(w, r)
	})
	opts := []otelchi.Option{
		otelchi.WithFilter(func(r *http.Request) bool {
			if r.URL.Path == "/healthz" {
				return false
			}
			return true
		}),
		otelchi.WithRequestMethodInSpanName(true),
		otelchi.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(buildSampler(p.config.Sampler)))),
	}

	return otelchi.Middleware(p.serverName(), opts...)(wrappedNext)
}

func (p *Plugin) serverName() string {
	if p.config.ServerName != "" {
		return p.config.ServerName
	}
	return "APISIX"
}

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

func (p *Plugin) additionalSpanAttributes(r *http.Request) []attribute.KeyValue {
	attrs := make(
		[]attribute.KeyValue,
		0,
		len(p.config.AdditionalAttributes)+len(p.config.AdditionalHeaderPrefixAttributes),
	)
	for _, name := range p.config.AdditionalAttributes {
		if value, ok := requestVariable(r, name); ok {
			attrs = append(attrs, attribute.String(name, value))
		}
	}

	headers := normalizedHeaders(r.Header)
	for _, key := range p.config.AdditionalHeaderPrefixAttributes {
		key = strings.ToLower(key)
		if strings.HasSuffix(key, "*") && len(key) > 1 {
			prefix := strings.TrimSuffix(key, "*")
			for header, value := range headers {
				if strings.HasPrefix(header, prefix) && value != "" {
					attrs = append(attrs, attribute.String(header, value))
				}
			}
			continue
		}

		if value := headers[key]; value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	return attrs
}

func requestVariable(r *http.Request, name string) (string, bool) {
	key := "$" + strings.TrimPrefix(name, "$")
	if value := v.GetNginxVar(r, key); value != "" {
		return value, true
	}
	if value, ok := coerceAttributeValue(apisixctx.GetApisixVar(r, key)); ok {
		return value, true
	}
	if value, ok := coerceAttributeValue(apisixctx.GetRequestVar(r, key)); ok {
		return value, true
	}
	return "", false
}

func coerceAttributeValue(value any) (string, bool) {
	if value == nil {
		return "", false
	}
	text := fmt.Sprint(value)
	if text == "" {
		return "", false
	}
	return text, true
}

func normalizedHeaders(headers http.Header) map[string]string {
	values := make(map[string]string, len(headers))
	for key, headerValues := range headers {
		if len(headerValues) == 0 {
			continue
		}
		values[strings.ToLower(key)] = strings.Join(headerValues, ", ")
	}
	return values
}
