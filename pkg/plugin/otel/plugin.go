package otel

import (
	"fmt"
	"net/http"

	"github.com/riandyrn/otelchi"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

const (
	// version  = "0.1"
	priority = 104
	name     = "otel"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "server_name": {
		"type": "string"
	  }
	},
	"required": [
	  "server_name"
	]
  }
`

type Plugin struct {
	base.BasePlugin
	config Config
}

type Config struct{}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	opts := otelchi.WithFilter(func(r *http.Request) bool {
		if r.URL.Path == "/healthz" {
			return false
		}
		return true
	})

	fmt.Print("init the otel plugin\n")

	return otelchi.Middleware("the_instance_id", opts)(next)
}
