package request_context

import (
	"net/http"
	"net/url"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixid "github.com/wklken/apisix-go/pkg/apisix/id"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
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
		metricsEnabled := metrics.HTTPRequestMetricsEnabled()
		returnedNormally := false
		var capture *base.ResponseCapture
		if direct && metricsEnabled {
			w, capture = base.CaptureResponseOutcomeController(w)
			_ = lifecycle.AddFinalizer(name, func() error {
				if !returnedNormally {
					metrics.Requests.Inc()
					return nil
				}
				request := lifecycle.FinalRequest()
				if request == nil {
					request = r
				}
				response := base.ResponseCaptureSnapshot{}
				if capture != nil {
					response = capture.Snapshot()
				}
				snapshotRequest := request.Clone(request.Context())
				snapshotRequest.Body = http.NoBody
				snapshot := base.BuildLogSnapshot(
					snapshotRequest,
					response,
					lifecycle.Outcome(),
					lifecycle.ResponseSource(),
					lifecycle.StartedAt(),
					lifecycle.FinishedAt(),
				)
				return p.RunSnapshotFinalizer(snapshot)
			})
		} else if !direct && metricsEnabled {
			// Handler remains a named direct-package compatibility adapter. The
			// production request-phase owner invokes RunSnapshotFinalizer through
			// the log executor; this fallback serves callers that still wrap the
			// handler around an externally managed lifecycle.
			_ = lifecycle.AddFinalizer(name, func() error {
				request := lifecycle.FinalRequest()
				if request == nil {
					request = r
				}
				snapshotRequest := request.Clone(request.Context())
				snapshotRequest.Body = http.NoBody
				return p.RunSnapshotFinalizer(base.BuildLogSnapshot(
					snapshotRequest,
					base.ResponseCaptureSnapshot{},
					lifecycle.Outcome(),
					lifecycle.ResponseSource(),
					lifecycle.StartedAt(),
					lifecycle.FinishedAt(),
				))
			})
		}
		if direct {
			defer func() {
				if capture != nil {
					lifecycle.SetOutcome(capture.Outcome())
				}
				lifecycle.Finalize()
				ctx.RecycleVars(r)
			}()
		}

		r = p.initializeRequest(r, lifecycle)
		next.ServeHTTP(w, r)
		returnedNormally = true
	}
	return http.HandlerFunc(fn)
}

// RunRequestPhase initializes shared request state. The outer request owner
// creates/finalizes the lifecycle and invokes RunSnapshotFinalizer after all
// response phases have published the detached final snapshot.
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

// RunSnapshotFinalizer records request metrics from a detached snapshot. It
// deliberately does not consult a live request or register another lifecycle
// callback; the log executor owns exactly-once callback ordering.
func (p *Plugin) RunSnapshotFinalizer(snapshot base.LogSnapshot) error {
	if !metrics.HTTPRequestMetricsEnabled() {
		return nil
	}
	request := requestFromSnapshot(snapshot)
	labels := p.metricLabels()
	requestLatency := snapshot.Finished.Sub(snapshot.Started).Milliseconds()
	if snapshot.Started.IsZero() || snapshot.Finished.IsZero() || requestLatency < 0 {
		requestLatency = 0
	}
	consumer := snapshot.Request.Consumer.Username
	node := snapshotStringVar(snapshot.Request.APISIXVars, "$balancer_ip")
	upstreamLatency := snapshotInt64Var(snapshot.Request.RequestVars, "$upstream_latency")
	ingressBytes := max(snapshot.Request.ContentLength, int64(0))
	metrics.Requests.Inc()
	metrics.RecordHTTPRequest(request, metrics.HTTPRequestMetrics{
		Status:          snapshot.Outcome.Status,
		Route:           labels.route,
		MatchedURI:      snapshotStringVar(snapshot.Request.APISIXVars, "$matched_uri"),
		MatchedHost:     snapshotStringVar(snapshot.Request.APISIXVars, "$matched_host"),
		Service:         labels.service,
		Consumer:        consumer,
		Node:            node,
		RequestLatency:  requestLatency,
		UpstreamLatency: upstreamLatency,
		IngressBytes:    ingressBytes,
		EgressBytes:     snapshot.Outcome.Bytes,
	})
	return nil
}

func requestFromSnapshot(snapshot base.LogSnapshot) *http.Request {
	requestURL, err := url.Parse(snapshot.Request.URL)
	if err != nil || requestURL == nil {
		requestURL, _ = url.Parse(snapshot.Request.URI)
	}
	if requestURL == nil {
		requestURL = &url.URL{}
	}
	request := &http.Request{
		Method:        snapshot.Request.Method,
		URL:           requestURL,
		Host:          snapshot.Request.Host,
		RemoteAddr:    snapshot.Request.RemoteAddr,
		Proto:         snapshot.Request.Proto,
		Header:        snapshot.Request.Header.Clone(),
		ContentLength: snapshot.Request.ContentLength,
		Body:          http.NoBody,
	}
	request = ctx.WithApisixVars(request, nil)
	request = ctx.WithRequestVars(request)
	for key, value := range snapshot.Request.APISIXVars {
		ctx.RegisterApisixVar(request, key, value)
	}
	for key, value := range snapshot.Request.RequestVars {
		ctx.RegisterRequestVar(request, key, value)
	}
	if snapshot.Source != ctx.ResponseSourceUnknown {
		ctx.RegisterRequestVar(request, "$response_source", string(snapshot.Source))
	}
	return request
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
