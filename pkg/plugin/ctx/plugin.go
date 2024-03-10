package basic_auth

import (
	"context"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

type Plugin struct {
	base.BasePlugin
	config Config
	data   map[string]interface{}
}

const (
	// version  = "0.1"
	priority = 1
	name     = "context"
)

const schema = `{}`

type Config struct{}

func New(r resource.Route) *Plugin {
	data := make(map[string]interface{})
	data["route_id"] = r.ID
	data["route_name"] = r.Name
	data["service_id"] = r.ServiceID
	// FIXME: add more context data here

	return &Plugin{
		BasePlugin: base.BasePlugin{
			Name:     name,
			Priority: priority,
			Schema:   schema,
		},
		config: Config{},
		data:   data,
	}
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
		// TODO: set the context into request

		// Set the context into request
		ctx := r.Context()
		for key, value := range p.data {
			ctx = context.WithValue(ctx, key, value)
		}
		r = r.WithContext(ctx)

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
