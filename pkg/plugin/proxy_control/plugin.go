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

const (
	// DefaultRequestBufferingLimit is the memory threshold for request replay.
	// Larger bodies use a request-owned temporary file rather than being rejected.
	DefaultRequestBufferingLimit int64 = 8 << 20
)

type requestBufferingState struct {
	enabled bool
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
	state := requestBufferingState{enabled: enabled}
	ctx := context.WithValue(r.Context(), requestBufferingKey{}, state)
	return r.WithContext(ctx)
}

func GetRequestBuffering(r *http.Request) bool {
	state, configured := r.Context().Value(requestBufferingKey{}).(requestBufferingState)
	return !configured || state.enabled
}
