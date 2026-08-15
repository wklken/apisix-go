package ai

import (
	"errors"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 22900
	name     = "ai"
)

const schema = `
{
  "type": "object",
  "properties": {}
}
`

var errUnsupportedControlPlane = errors.New(
	"ai plugin is unsupported: control-plane AI runtime is not implemented",
)

type Config struct{}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	return errUnsupportedControlPlane
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
