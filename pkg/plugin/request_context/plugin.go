package request_context

import (
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
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
		lifecycle := ctx.GetRequestLifecycle(r)
		direct := lifecycle == nil
		r, lifecycle = ctx.EnsureRequestLifecycle(r, time.Now())
		metricsEnabled := metrics.HTTPRequestMetricsEnabled()
		returnedNormally := false
		var snapshot func() ctx.ResponseOutcome
		if direct && metricsEnabled {
			w, snapshot, _ = base.CaptureResponseOutcome(w)
		}
		if direct {
			defer func() {
				if snapshot != nil {
					lifecycle.SetOutcome(snapshot())
				}
				lifecycle.Finalize()
				ctx.RecycleVars(r)
			}()
		}

		r = p.initializeRequest(r, lifecycle, direct, &returnedNormally)
		next.ServeHTTP(w, r)
		returnedNormally = true
	}
	return http.HandlerFunc(fn)
}

// RunRequestPhase initializes shared request state and registers the existing
// request metrics finalizer. The outer request owner creates/finalizes the
// lifecycle and recycles request state after all finalizers run.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	lifecycle := ctx.GetRequestLifecycle(r)
	r = p.initializeRequest(r, lifecycle, false, nil)
	return base.ContinueRequest(r)
}

func (p *Plugin) initializeRequest(
	r *http.Request,
	lifecycle *ctx.RequestLifecycle,
	direct bool,
	returnedNormally *bool,
) *http.Request {
	r = ctx.WithApisixVars(r, map[string]string{
		"$route_id":     p.config.RouteID,
		"$route_name":   p.config.RouteName,
		"$matched_uri":  p.config.MatchedURI,
		"$matched_host": p.config.MatchedHost,
		"$service_id":   p.config.ServiceID,
		"$service_name": p.config.ServiceName,
	})
	r = ctx.WithRequestVars(r)

	if !metrics.HTTPRequestMetricsEnabled() || lifecycle == nil {
		return r
	}

	labels := p.metricLabels()
	startedAt := lifecycle.StartedAt()
	_ = lifecycle.AddFinalizer(name, func() error {
		if direct && (returnedNormally == nil || !*returnedNormally) {
			metrics.Requests.Inc()
			return nil
		}
		request := lifecycle.FinalRequest()
		if request == nil {
			request = r
		}
		outcome := lifecycle.Outcome()
		consumer := apisixStringVar(request, "$consumer_name")
		node := apisixStringVar(request, "$balancer_ip")
		upstreamLatency := requestInt64Var(request, "$upstream_latency")
		metrics.Requests.Inc()
		metrics.RecordHTTPRequest(request, metrics.HTTPRequestMetrics{
			Status:          outcome.Status,
			Route:           labels.route,
			MatchedURI:      apisixStringVar(request, "$matched_uri"),
			MatchedHost:     apisixStringVar(request, "$matched_host"),
			Service:         labels.service,
			Consumer:        consumer,
			Node:            node,
			RequestLatency:  time.Since(startedAt).Milliseconds(),
			UpstreamLatency: upstreamLatency,
			IngressBytes:    util.RequestSize(request),
			EgressBytes:     outcome.Bytes,
		})
		return nil
	})
	return r
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
