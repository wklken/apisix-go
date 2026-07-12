package proxy_buffering

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
	priority = 21991
	name     = "proxy-buffering"
)

const schema = `
{
  "type": "object",
  "properties": {
    "disable_proxy_buffering": {
      "type": "boolean",
      "default": false
    }
  }
}
`

type Config struct {
	DisableProxyBuffering bool `json:"disable_proxy_buffering,omitempty"`
}

type disableProxyBufferingKey struct{}

type flushingResponseWriter struct {
	http.ResponseWriter
	flusher http.Flusher
}

func (w *flushingResponseWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	if err == nil {
		w.flusher.Flush()
	}
	return n, err
}

func (w *flushingResponseWriter) Flush() {
	w.flusher.Flush()
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
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		r = WithDisableProxyBuffering(r, p.config.DisableProxyBuffering)
		if p.config.DisableProxyBuffering {
			if flusher, ok := w.(http.Flusher); ok {
				w = &flushingResponseWriter{ResponseWriter: w, flusher: flusher}
			}
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func WithDisableProxyBuffering(r *http.Request, disabled bool) *http.Request {
	ctx := context.WithValue(r.Context(), disableProxyBufferingKey{}, disabled)
	return r.WithContext(ctx)
}

func GetDisableProxyBuffering(r *http.Request) bool {
	disabled, _ := r.Context().Value(disableProxyBufferingKey{}).(bool)
	return disabled
}
