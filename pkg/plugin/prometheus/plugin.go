package prometheus

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 500
	name     = "prometheus"

	MetricsURI = "/apisix/prometheus/metrics"
)

const schema = `
{
  "type": "object",
  "properties": {
    "prefer_name": {
      "type": "boolean",
      "default": false
    }
  }
}
`

type Config struct {
	PreferName bool `json:"prefer_name,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	public_api.Register(http.MethodGet, MetricsURI, http.HandlerFunc(MetricsHandler))
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}
