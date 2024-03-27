package client_control

import (
	"bytes"
	"io"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 22000
	name     = "client-control"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "max_body_size": {
		"type": "integer",
		"minimum": 0,
		"description": "Maximum message body size in bytes. No restriction when set to 0."
	  }
	}
}`

type Config struct {
	MaxBodySize int64 `json:"max_body_size"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.MaxBodySize > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, p.config.MaxBodySize)

			// TODO: maybe a question here? we read the body
			body, err := io.ReadAll(r.Body)
			if err != nil {
				if err.Error() == "http: request body too large" {
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					return
				}
				// FIXME: handle other errors
			}

			// reset the r.Body
			r.Body = io.NopCloser(bytes.NewReader(body))

			next.ServeHTTP(w, r)
		} else {
			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}
