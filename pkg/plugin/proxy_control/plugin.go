package proxy_control

import (
	"context"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 21990
	name     = "proxy-control"
)

const schema = `
{
  "type": "object",
  "properties": {
    "request_buffering": {
      "type": "boolean",
      "default": true
    }
  }
}
`

type Config struct {
	RequestBuffering *bool `json:"request_buffering,omitempty"`
}

type requestBufferingKey struct{}

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
	if p.config.RequestBuffering == nil {
		requestBuffering := true
		p.config.RequestBuffering = &requestBuffering
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		r = WithRequestBuffering(r, *p.config.RequestBuffering)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func WithRequestBuffering(r *http.Request, enabled bool) *http.Request {
	ctx := context.WithValue(r.Context(), requestBufferingKey{}, enabled)
	return r.WithContext(ctx)
}

func GetRequestBuffering(r *http.Request) bool {
	enabled, _ := r.Context().Value(requestBufferingKey{}).(bool)
	return enabled
}
