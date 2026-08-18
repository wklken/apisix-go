package request_context

import (
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixid "github.com/wklken/apisix-go/pkg/apisix/id"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	nodeID string
}

const (
	// version  = "0.1"
	priority = 9999999
	name     = "request_context"
)

const schema = `{}`

type Config struct {
	RouteID     string `json:"$route_id"`
	RouteName   string `json:"$route_name"`
	MatchedURI  string `json:"$matched_uri"`
	MatchedHost string `json:"$matched_host"`
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
	p.nodeID = apisixid.Get()
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		lifecycle := ctx.GetRequestLifecycle(r)
		direct := lifecycle == nil
		r, lifecycle = ctx.EnsureRequestLifecycle(r, time.Now())
		if direct {
			defer func() {
				lifecycle.Finalize()
				ctx.RecycleVars(r)
			}()
		}

		r = p.initializeRequest(r, lifecycle)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

// RunRequestPhase initializes shared request state. The outer request owner
// creates and finalizes the lifecycle. The main server owns the global request
// gauge and the effective Prometheus log binding owns route-level metrics.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	lifecycle := ctx.GetRequestLifecycle(r)
	r = p.initializeRequest(r, lifecycle)
	return base.ContinueRequest(r)
}

func (p *Plugin) initializeRequest(r *http.Request, lifecycle *ctx.RequestLifecycle) *http.Request {
	r = ctx.WithApisixVars(r, map[string]string{
		"$node_id":      p.nodeID,
		"$route_id":     p.config.RouteID,
		"$route_name":   p.config.RouteName,
		"$matched_uri":  p.config.MatchedURI,
		"$matched_host": p.config.MatchedHost,
		"$service_id":   p.config.ServiceID,
		"$service_name": p.config.ServiceName,
	})
	r = ctx.WithRequestVars(r)

	return r
}
