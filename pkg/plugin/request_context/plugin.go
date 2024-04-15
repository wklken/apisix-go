package request_context

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 0
	name     = "request_context"
)

const schema = `{}`

type Config struct {
	RouteID     string `json:"$route_id"`
	RouteName   string `json:"$route_name"`
	ServiceID   string `json:"$service_id"`
	ServiceName string `json:"$service_name"`
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
		r = ctx.WithApisixVars(r, map[string]string{
			"$route_id":     p.config.RouteID,
			"$route_name":   p.config.RouteName,
			"$service_id":   p.config.ServiceID,
			"$service_name": p.config.ServiceName,
		})
		r = ctx.WithRequestVars(r)

		// just init the request vars
		next.ServeHTTP(w, r)

		ctx.RecycleVars(r)
	}
	return http.HandlerFunc(fn)
}
