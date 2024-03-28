package proxy_rewrite

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
	// version  = "0.1"
	priority = 1008
	name     = "proxy-rewrite"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "uri": {
		"type": "string"
	  },
	  "method": {
		"type": "string"
	  },
	  "host": {
		"type": "string"
	  },
	  "scheme": {
		"type": "string"
	  },
	  "headers": {
		"type": "object",
		"properties": {
			"add": {
				"type": "object"
			},
			"set": {
				"type": "object"
			},
			"remove": {
				"type": "array"
			}
		}
	  }
	}
  }
`

type Headers struct {
	Add    map[string]string `json:"add"`
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
}

type Config struct {
	Uri     string  `json:"uri"`
	Method  string  `json:"method"`
	Host    string  `json:"host"`
	Scheme  string  `json:"scheme"`
	Headers Headers `json:"headers"`
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

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		data := map[string]interface{}{
			"uri":     p.config.Uri,
			"method":  p.config.Method,
			"host":    p.config.Host,
			"scheme":  p.config.Scheme,
			"headers": p.config.Headers,
		}

		ctx = context.WithValue(ctx, "proxy-rewrite", data)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}
