package request_context

import (
	"net/http"

	"github.com/felixge/httpsnoop"
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
	MatchedHost          string `json:"$matched_host"`
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

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		labels := p.metricLabels()

		metrics.Requests.Inc()

		r = ctx.WithApisixVars(r, map[string]string{
			"$route_id":     p.config.RouteID,
			"$route_name":   p.config.RouteName,
			"$matched_uri":  p.config.MatchedURI,
			"$matched_host": p.config.MatchedHost,
			"$service_id":   p.config.ServiceID,
			"$service_name": p.config.ServiceName,
		})
		r = ctx.WithRequestVars(r)

		captured := httpsnoop.CaptureMetrics(next, w, r)
		consumer := apisixStringVar(r, "$consumer_name")
		node := apisixStringVar(r, "$balancer_ip")
		latency := captured.Duration.Milliseconds()
		upstreamLatency := requestInt64Var(r, "$upstream_latency")

		metrics.RecordHTTPRequest(r, metrics.HTTPRequestMetrics{
			Status:          captured.Code,
			Route:           labels.route,
			MatchedURI:      apisixStringVar(r, "$matched_uri"),
			MatchedHost:     apisixStringVar(r, "$matched_host"),
			Service:         labels.service,
			Consumer:        consumer,
			Node:            node,
			RequestLatency:  latency,
			UpstreamLatency: upstreamLatency,
			IngressBytes:    requestSize(r),
			EgressBytes:     captured.Written,
		})

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

func apisixStringVar(r *http.Request, key string) string {
	value, _ := ctx.GetApisixVar(r, key).(string)
	return value
}

func requestInt64Var(r *http.Request, key string) int64 {
	switch value := ctx.GetRequestVar(r, key).(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}

func requestSize(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}
