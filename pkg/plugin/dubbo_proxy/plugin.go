package dubbo_proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 507
	name     = "dubbo-proxy"

	ctxEnabled        contextKey = "dubbo_proxy_enabled"
	ctxServiceName    contextKey = "dubbo_service_name"
	ctxServiceVersion contextKey = "dubbo_service_version"
	ctxMethod         contextKey = "dubbo_method"
)

const schema = `
{
  "type": "object",
  "properties": {
    "service_name": {
      "type": "string",
      "minLength": 1
    },
    "service_version": {
      "type": "string",
      "pattern": "^\\d+\\.\\d+\\.\\d+"
    },
    "method": {
      "type": "string",
      "minLength": 1
    }
  },
  "required": ["service_name", "service_version"]
}
`

type Config struct {
	ServiceName    string `json:"service_name"`
	ServiceVersion string `json:"service_version"`
	Method         string `json:"method,omitempty"`
	MultiplexCount int    `json:"-"`
}

type contextKey string

type configKey struct{}

func WithConfig(r *http.Request, cfg Config) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), configKey{}, cfg))
}

func GetConfig(r *http.Request) (Config, bool) {
	cfg, ok := r.Context().Value(configKey{}).(Config)
	return cfg, ok
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	multiplexCount, err := loadMultiplexCount()
	if err != nil {
		return err
	}
	p.config.MultiplexCount = multiplexCount
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		method := p.config.Method
		if method == "" {
			method = strings.TrimPrefix(r.URL.Path, "/")
		}
		cfg := p.config
		cfg.Method = method

		ctx := context.WithValue(r.Context(), ctxEnabled, true)
		ctx = context.WithValue(ctx, ctxServiceName, p.config.ServiceName)
		ctx = context.WithValue(ctx, ctxServiceVersion, p.config.ServiceVersion)
		ctx = context.WithValue(ctx, ctxMethod, method)
		next.ServeHTTP(w, WithConfig(r.WithContext(ctx), cfg))
	}
	return http.HandlerFunc(fn)
}

func Enabled(r *http.Request) bool {
	enabled, _ := r.Context().Value(ctxEnabled).(bool)
	return enabled
}

func ServiceName(r *http.Request) string {
	serviceName, _ := r.Context().Value(ctxServiceName).(string)
	return serviceName
}

func ServiceVersion(r *http.Request) string {
	serviceVersion, _ := r.Context().Value(ctxServiceVersion).(string)
	return serviceVersion
}

func Method(r *http.Request) string {
	method, _ := r.Context().Value(ctxMethod).(string)
	return method
}
