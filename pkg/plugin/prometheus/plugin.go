package prometheus

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

type Plugin struct {
	base.BasePlugin
	config  Config
	route   resource.Route
	service resource.Service
}

const (
	priority = 500
	name     = "prometheus"

	MetricsURI = "/apisix/prometheus/metrics"
)

const schema = `
{
  "type": "object",
  "properties": {
    "prefer_name": {
      "type": "boolean",
      "default": false
    }
  }
}
`

type Config struct {
	PreferName bool `json:"prefer_name,omitempty"`
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

// SetResourceContext supplies route/service identity that is not part of the
// user-facing Prometheus plugin schema. The builder calls this for every
// materialized binding, including global bindings evaluated for a route.
func (p *Plugin) SetResourceContext(route resource.Route, service resource.Service) {
	p.route = route
	p.service = service
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// RunLogPhase owns request metrics for an effective Prometheus binding. The
// log executor invokes this callback once after response completion (including
// APISIX-generated responses), while the main server owns the process-level
// request-total gauge and the route variable initializer remains metrics-free.
func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	if !metrics.HTTPRequestMetricsEnabled() {
		return nil
	}
	requestLatency := snapshot.Finished.Sub(snapshot.Started).Milliseconds()
	if snapshot.Started.IsZero() || snapshot.Finished.IsZero() || requestLatency < 0 {
		requestLatency = 0
	}
	consumer := snapshot.Request.Consumer.Username
	node := snapshotStringVar(snapshot.Request.APISIXVars, "$balancer_ip")
	upstreamLatency := snapshotInt64Var(snapshot.Request.RequestVars, "$upstream_latency")
	ingressBytes := max(snapshot.Request.ContentLength, int64(0))
	routeID, routeName := p.route.ID, p.route.Name
	serviceID, serviceName := p.service.ID, p.service.Name
	if routeID == "" {
		routeID = snapshotStringVar(snapshot.Request.APISIXVars, "$route_id")
	}
	if routeName == "" {
		routeName = snapshotStringVar(snapshot.Request.APISIXVars, "$route_name")
	}
	if serviceID == "" {
		serviceID = snapshotStringVar(snapshot.Request.APISIXVars, "$service_id")
	}
	if serviceName == "" {
		serviceName = snapshotStringVar(snapshot.Request.APISIXVars, "$service_name")
	}
	metrics.RecordHTTPRequestContext(metrics.HTTPRequestMetricContext{
		Method:         snapshot.Request.Method,
		Host:           snapshot.Request.Host,
		Path:           snapshot.Request.Path,
		APISIXVars:     snapshot.Request.APISIXVars,
		RequestVars:    snapshot.Request.RequestVars,
		ResponseSource: snapshot.Source,
	}, metrics.HTTPRequestMetrics{
		Status: snapshot.Outcome.Status,
		Route: metricResourceLabel(
			routeID,
			routeName,
			p.config.PreferName,
		),
		MatchedURI:  snapshotStringVar(snapshot.Request.APISIXVars, "$matched_uri"),
		MatchedHost: snapshotStringVar(snapshot.Request.APISIXVars, "$matched_host"),
		Service: metricResourceLabel(
			serviceID,
			serviceName,
			p.config.PreferName,
		),
		Consumer:        consumer,
		Node:            node,
		RequestLatency:  requestLatency,
		UpstreamLatency: upstreamLatency,
		IngressBytes:    ingressBytes,
		EgressBytes:     snapshot.Outcome.Bytes,
	})
	return nil
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

func snapshotStringVar(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func snapshotInt64Var(values map[string]any, key string) int64 {
	switch value := values[key].(type) {
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

// scrapeHandler is built once and reused for every scrape; building it per
// request is pure setup cost on the metrics endpoint.
var scrapeHandler = sync.OnceValue(promhttp.Handler)

func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	scrapeHandler().ServeHTTP(w, r)
}
