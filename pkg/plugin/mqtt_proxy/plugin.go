package mqtt_proxy

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 1000
	name     = "mqtt-proxy"
)

const schema = `
{
  "type": "object",
  "properties": {
    "protocol_name": {
      "type": "string",
      "default": "MQTT"
    },
    "protocol_level": {
      "type": "integer"
    }
  },
  "required": ["protocol_level"]
}
`

type Config struct {
	ProtocolName  string `json:"protocol_name,omitempty"`
	ProtocolLevel int    `json:"protocol_level,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.ProtocolName == "" {
		p.config.ProtocolName = "MQTT"
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
