package request_context

import (
	"net/http"
	"time"

	"github.com/spf13/cast"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 9999999
	name     = "request_context"
)

const schema = `{}`

type Config struct {
	RouteID              string `json:"$route_id"`
	RouteName            string `json:"$route_name"`
	MatchedURI           string `json:"$matched_uri"`
	ServiceID            string `json:"$service_id"`
	ServiceName          string `json:"$service_name"`
	PrometheusPreferName bool   `json:"$prometheus_prefer_name"`
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
		labels := p.metricLabels()

		// metrics
		metrics.Requests.Inc()
		begin := time.Now()

		metrics.Bandwidth.WithLabelValues(
			"ingress",
			labels.route,
			labels.service,
			"",
			"127.0.0.1",
		).Add(float64(r.ContentLength))

		r = ctx.WithApisixVars(r, map[string]string{
			"$route_id":     p.config.RouteID,
			"$route_name":   p.config.RouteName,
			"$matched_uri":  p.config.MatchedURI,
			"$service_id":   p.config.ServiceID,
			"$service_name": p.config.ServiceName,
		})
		r = ctx.WithRequestVars(r)

		// just init the request vars
		next.ServeHTTP(w, r)

		latency := time.Since(begin).Milliseconds()
		metrics.HttpLatency.WithLabelValues(
			"request",
			labels.route,
			labels.service,
			"",
			"127.0.0.1",
		).Observe(float64(latency))
		metrics.HttpStatus.WithLabelValues(
			cast.ToString(ctx.GetRequestVar(r, "$status")),
			labels.route,
			ctx.GetApisixVar(r, "$matched_uri").(string),
			"",
			labels.service,
			"",
			"127.0.0.1",
		).Inc()

		ctx.RecycleVars(r)
	}
	return http.HandlerFunc(fn)
}

type metricLabels struct {
	route   string
	service string
}

func (p *Plugin) metricLabels() metricLabels {
	return metricLabels{
		route:   metricResourceLabel(p.config.RouteID, p.config.RouteName, p.config.PrometheusPreferName),
		service: metricResourceLabel(p.config.ServiceID, p.config.ServiceName, p.config.PrometheusPreferName),
	}
}

func metricResourceLabel(id string, name string, preferName bool) string {
	if preferName && name != "" {
		return name
	}
	if id != "" {
		return id
	}
	return name
}
