package skywalking

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-resty/resty/v2"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *resty.Client

	clientRelease func()

	sampleRandom func() (float64, error)

	reportMu    sync.Mutex
	reportWake  chan struct{}
	reportFlush chan chan struct{}
	reportDone  chan struct{}
	segments    []skywalkingSegment
	dropped     int
	stopped     bool
}

const (
	priority = 12010
	name     = "skywalking"

	componentIDAPISIX = 6002
	upstreamService   = "upstream service"

	// maxPendingSkyWalkingSegments bounds the queued segment window so a
	// failing collector cannot grow memory without limit.
	maxPendingSkyWalkingSegments = 1000

	skywalkingReportTimeout = 5 * time.Second
)

const schema = `
{
  "type": "object",
  "properties": {
    "sample_ratio": {
      "type": "number",
      "minimum": 0.00001,
      "maximum": 1,
      "default": 1
    }
  }
}
`

type Config struct {
	SampleRatio         float64 `json:"sample_ratio,omitempty"`
	ServiceName         string  `json:"service_name,omitempty"`
	ServiceInstanceName string  `json:"service_instance_name,omitempty"`
	EndpointAddr        string  `json:"endpoint_addr,omitempty"`
	ReportInterval      int     `json:"report_interval,omitempty"`
}

type tracingContextKey struct{}

type segmentStateContextKey struct{}

type segmentState struct {
	context             sw8Context
	started             time.Time
	originalSW8         []string
	owner               *Plugin
	sampled             bool
	finalizerRegistered bool
	once                sync.Once
}

type sw8Context struct {
	TraceID              string
	TraceSegmentID       string
	SpanID               int
	ParentTraceSegmentID string
	ParentSpanID         int
	ParentService        string
	ParentInstance       string
	ParentEndpoint       string
	AddressUsedAtClient  string
}

type skywalkingSegment struct {
	TraceID         string           `json:"traceId"`
	TraceSegmentID  string           `json:"traceSegmentId"`
	Service         string           `json:"service"`
	ServiceInstance string           `json:"serviceInstance"`
	Spans           []skywalkingSpan `json:"spans"`
}

type skywalkingSegmentRef struct {
	RefType                  string `json:"refType"`
	TraceID                  string `json:"traceId"`
	ParentTraceSegmentID     string `json:"parentTraceSegmentId"`
	ParentSpanID             int    `json:"parentSpanId"`
	ParentService            string `json:"parentService"`
	ParentServiceInstance    string `json:"parentServiceInstance"`
	ParentEndpoint           string `json:"parentEndpoint"`
	NetworkAddressUsedAtPeer string `json:"networkAddressUsedAtPeer"`
}

type skywalkingTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type skywalkingSpan struct {
	SpanID        int                    `json:"spanId"`
	ParentSpanID  int                    `json:"parentSpanId"`
	OperationName string                 `json:"operationName"`
	StartTime     int64                  `json:"startTime"`
	EndTime       int64                  `json:"endTime"`
	SpanType      string                 `json:"spanType"`
	SpanLayer     string                 `json:"spanLayer"`
	ComponentID   int                    `json:"componentId"`
	IsError       bool                   `json:"isError"`
	Refs          []skywalkingSegmentRef `json:"refs,omitempty"`
	Tags          []skywalkingTag        `json:"tags,omitempty"`
	Logs          []map[string]string    `json:"logs,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if err := p.loadPluginAttr(); err != nil {
		return err
	}
	p.applyDefaults()
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.config.EndpointAddr)), "http://") {
		logger.Warn("Using skywalking endpoint_addr with no TLS is a security risk")
	}
	if p.sampleRandom == nil {
		p.sampleRandom = func() (float64, error) { return randomUnit(rand.Reader) }
	}
	owner := p.TaskOwner()
	if owner == nil {
		return runtime.ErrTaskOwnerRequired
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.EndpointAddr)
	client := resty.New()
	client.SetTimeout(skywalkingReportTimeout)
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) { return client, nil },
		shared.CloseRestyClient,
	)
	if err != nil {
		return err
	}
	p.client = value.(*resty.Client)
	p.clientRelease = release
	p.reportWake = make(chan struct{}, 1)
	p.reportFlush = make(chan chan struct{})
	p.reportDone = make(chan struct{})
	if err := owner.Go("segment-reporter", p.reporterLoop); err != nil {
		release()
		p.client = nil
		p.clientRelease = nil
		p.reportWake = nil
		p.reportFlush = nil
		p.reportDone = nil
		return err
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// Production route assembly invokes RunRequestPhase. This direct
		// compatibility adapter must not duplicate a span when an outer
		// lifecycle has already installed the request-stage owner.
		if apisixctx.GetRequestLifecycle(r) != nil || r.Context().Value(segmentStateContextKey{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		if r.Context().Value(tracingContextKey{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), tracingContextKey{}, struct{}{}))
		sample, err := p.shouldSample()
		if err != nil {
			http.Error(w, "failed to generate sampling decision", http.StatusInternalServerError)
			return
		}
		if !sample {
			next.ServeHTTP(w, r)
			return
		}

		ctx, _ := parseSW8(r.Header.Get("sw8"))
		if ctx.TraceID == "" {
			traceID, err := randomID(rand.Reader, 16)
			if err != nil {
				http.Error(w, "failed to generate trace id", http.StatusInternalServerError)
				return
			}
			ctx.TraceID = traceID
		}
		if ctx.TraceSegmentID == "" {
			segmentID, err := randomID(rand.Reader, 16)
			if err != nil {
				http.Error(w, "failed to generate trace segment id", http.StatusInternalServerError)
				return
			}
			ctx.TraceSegmentID = segmentID
		}
		ctx.SpanID = 1
		r.Header.Set("sw8", ctx.header(p.config.ServiceName, p.serviceInstanceName(), r.URL.Path))

		start := time.Now()
		captured := httpsnoop.CaptureMetrics(next, w, r)
		p.reportSegment(p.buildSegment(ctx, r, captured.Code, start, captured.Duration))
	}
	return http.HandlerFunc(fn)
}

// RunRequestPhase starts SkyWalking propagation at the inherited rewrite
// stage. Sampled requests register one lifecycle-owned segment finalizer;
// unsampled requests register none.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	if r == nil {
		return base.ContinueRequest(r)
	}
	state, exists := r.Context().Value(segmentStateContextKey{}).(*segmentState)
	lifecycle := apisixctx.GetRequestLifecycle(r)
	if lifecycle == nil {
		r, lifecycle = apisixctx.EnsureRequestLifecycle(r, time.Now())
	}
	if !exists {
		state = &segmentState{originalSW8: append([]string(nil), r.Header.Values("sw8")...)}
		r = r.WithContext(context.WithValue(r.Context(), segmentStateContextKey{}, state))
	} else {
		// A lower-precedence SkyWalking binding may already have generated an
		// outbound header. The final binding must make its sampling and parent
		// decision from the original client context, as if the earlier binding
		// had been skipped by APISIX's prefer_route policy.
		restoreSkyWalkingHeader(r, state.originalSW8)
	}
	traceContext, _ := parseSW8(r.Header.Get("sw8"))
	sample, err := p.shouldSample()
	if err != nil {
		http.Error(w, "failed to generate sampling decision", http.StatusInternalServerError)
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
	}
	if !sample {
		restoreSkyWalkingHeader(r, state.originalSW8)
		state.owner = p
		state.sampled = false
		return base.ContinueRequest(r)
	}
	if traceContext.TraceID == "" {
		traceContext.TraceID, err = randomID(rand.Reader, 16)
		if err != nil {
			http.Error(w, "failed to generate trace id", http.StatusInternalServerError)
			return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
		}
	}
	if traceContext.TraceSegmentID == "" {
		traceContext.TraceSegmentID, err = randomID(rand.Reader, 16)
		if err != nil {
			http.Error(w, "failed to generate trace segment id", http.StatusInternalServerError)
			return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
		}
	}
	traceContext.SpanID = 1
	r.Header.Set("sw8", traceContext.header(p.config.ServiceName, p.serviceInstanceName(), r.URL.Path))
	state.context = traceContext
	state.started = time.Now()
	state.owner = p
	state.sampled = true
	if !state.finalizerRegistered {
		if lifecycle.AddFinalizer(name, func() error {
			if !state.sampled || state.owner == nil {
				return nil
			}
			return state.owner.finishSegment(state, lifecycle, r)
		}) {
			state.finalizerRegistered = true
		}
	}
	return base.ContinueRequest(r)
}

func restoreSkyWalkingHeader(r *http.Request, original []string) {
	r.Header.Del("sw8")
	for _, value := range original {
		r.Header.Add("sw8", value)
	}
}

func (p *Plugin) finishSegment(
	state *segmentState,
	lifecycle *apisixctx.RequestLifecycle,
	fallback *http.Request,
) error {
	if state == nil || lifecycle == nil {
		return nil
	}
	state.once.Do(func() {
		request := lifecycle.FinalRequest()
		if request == nil {
			request = fallback
		}
		if request == nil {
			return
		}
		finished := lifecycle.FinishedAt()
		if finished.IsZero() {
			finished = time.Now()
		}
		duration := max(finished.Sub(state.started), time.Duration(0))
		outcome := lifecycle.Outcome()
		status := outcome.Status
		if status == 0 {
			status = http.StatusOK
		}
		p.reportSegment(p.buildSegmentWithSource(
			state.context,
			request,
			status,
			state.started,
			duration,
			lifecycle.ResponseSource(),
			outcome.Kind,
		))
	})
	return nil
}

func (p *Plugin) loadPluginAttr() error {
	effective := p.StaticConfig()
	if effective == nil {
		return fmt.Errorf("effective config is required")
	}
	attr := effective.Config.PluginAttr[name]
	if p.config.ServiceName == "" {
		p.config.ServiceName, _ = attr["service_name"].(string)
	}
	if p.config.ServiceInstanceName == "" {
		p.config.ServiceInstanceName, _ = attr["service_instance_name"].(string)
	}
	if p.config.EndpointAddr == "" {
		p.config.EndpointAddr, _ = attr["endpoint_addr"].(string)
	}
	if p.config.ReportInterval == 0 {
		p.config.ReportInterval = intFromAttr(attr, "report_interval")
	}
	return nil
}

func (p *Plugin) applyDefaults() {
	if p.config.SampleRatio == 0 {
		p.config.SampleRatio = 1
	}
	if p.config.ServiceName == "" {
		p.config.ServiceName = "APISIX"
	}
	if p.config.ServiceInstanceName == "" {
		p.config.ServiceInstanceName = "APISIX Instance Name"
	}
	if p.config.EndpointAddr == "" {
		p.config.EndpointAddr = "http://127.0.0.1:12800"
	}
	if p.config.ReportInterval == 0 {
		p.config.ReportInterval = 3
	}
}

func (p *Plugin) shouldSample() (bool, error) {
	if p.config.SampleRatio >= 1 {
		return true, nil
	}
	random, err := p.sampleRandom()
	if err != nil {
		return false, err
	}
	return random < p.config.SampleRatio, nil
}

func (p *Plugin) buildSegment(
	ctx sw8Context,
	r *http.Request,
	status int,
	start time.Time,
	duration time.Duration,
) skywalkingSegment {
	return p.buildSegmentWithSource(ctx, r, status, start, duration, apisixctx.ResponseSourceUnknown)
}

func (p *Plugin) buildSegmentWithSource(
	ctx sw8Context,
	r *http.Request,
	status int,
	start time.Time,
	duration time.Duration,
	source apisixctx.ResponseSource,
	outcomes ...apisixctx.RequestOutcomeKind,
) skywalkingSegment {
	end := start.Add(duration)
	tags := []skywalkingTag{
		{Key: "http.method", Value: r.Method},
		{Key: "http.url", Value: r.URL.RequestURI()},
		{Key: "http.status_code", Value: fmt.Sprint(status)},
	}
	if source != apisixctx.ResponseSourceUnknown {
		tags = append(tags, skywalkingTag{Key: "apisix.response_source", Value: string(source)})
	}
	correlation := apisixlog.CaptureRequestCorrelation(r)
	for _, tag := range []skywalkingTag{
		{Key: "apisix.request_id", Value: correlation.RequestID},
		{Key: "apisix.node_id", Value: correlation.NodeID},
		{Key: "apisix.retry_count", Value: correlation.RetryCount},
		{Key: "http.upstream_status_code", Value: correlation.UpstreamStatus},
	} {
		if tag.Value != "" {
			tags = append(tags, tag)
		}
	}
	if len(outcomes) > 0 && outcomes[0] != "" {
		tags = append(tags, skywalkingTag{Key: "apisix.outcome", Value: string(outcomes[0])})
	}
	span := skywalkingSpan{
		SpanID:        0,
		ParentSpanID:  -1,
		OperationName: r.Method + " " + r.URL.Path,
		StartTime:     start.UnixMilli(),
		EndTime:       end.UnixMilli(),
		SpanType:      "Entry",
		SpanLayer:     "Http",
		ComponentID:   componentIDAPISIX,
		IsError:       status >= 500,
		Tags:          tags,
	}
	segment := skywalkingSegment{
		TraceID:         ctx.TraceID,
		TraceSegmentID:  ctx.TraceSegmentID,
		Service:         p.config.ServiceName,
		ServiceInstance: p.serviceInstanceName(),
		Spans:           []skywalkingSpan{span},
	}
	if ctx.ParentTraceSegmentID != "" {
		span.Refs = []skywalkingSegmentRef{{
			RefType:                  "CrossProcess",
			TraceID:                  ctx.TraceID,
			ParentTraceSegmentID:     ctx.ParentTraceSegmentID,
			ParentSpanID:             ctx.ParentSpanID,
			ParentService:            ctx.ParentService,
			ParentServiceInstance:    ctx.ParentInstance,
			ParentEndpoint:           ctx.ParentEndpoint,
			NetworkAddressUsedAtPeer: ctx.AddressUsedAtClient,
		}}
		segment.Spans[0] = span
	}
	return segment
}

func (p *Plugin) reportSegment(segment skywalkingSegment) {
	p.reportMu.Lock()
	if p.stopped {
		p.reportMu.Unlock()
		return
	}
	if len(p.segments) >= maxPendingSkyWalkingSegments {
		p.dropped++
		p.reportMu.Unlock()
		return
	}
	p.segments = append(p.segments, segment)
	p.reportMu.Unlock()
	p.wakeReporter()
}

func (p *Plugin) Flush() {
	if p.reportFlush == nil || p.reportDone == nil {
		return
	}
	ack := make(chan struct{})
	select {
	case p.reportFlush <- ack:
		select {
		case <-ack:
		case <-p.reportDone:
		}
	case <-p.reportDone:
	}
}

// QuiesceGenerationTasks rejects new segments before the generation task
// registry cancels the reporter. The registry then owns task cancellation and
// joining; Stop only releases the shared client after that lifecycle step.
func (p *Plugin) QuiesceGenerationTasks() {
	p.reportMu.Lock()
	p.stopped = true
	p.reportMu.Unlock()
}

func (p *Plugin) Stop() {
	release := p.clientRelease
	p.clientRelease = nil
	p.client = nil
	if release != nil {
		release()
	}
}

func (p *Plugin) reporterLoop(ctx context.Context) error {
	defer close(p.reportDone)

	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		timerC = nil
	}

	for {
		select {
		case <-ctx.Done():
			stopTimer()
			p.reportMu.Lock()
			p.stopped = true
			p.reportMu.Unlock()
			p.flushSegments(context.WithoutCancel(ctx))
			return nil
		case <-p.reportWake:
			if timer == nil {
				timer = time.NewTimer(time.Duration(p.config.ReportInterval) * time.Second)
				timerC = timer.C
			}
		case ack := <-p.reportFlush:
			stopTimer()
			flushContext := ctx
			if ctx.Err() != nil {
				p.reportMu.Lock()
				p.stopped = true
				p.reportMu.Unlock()
				flushContext = context.WithoutCancel(ctx)
			}
			p.flushSegments(flushContext)
			close(ack)
		case <-timerC:
			timer = nil
			timerC = nil
			p.flushSegments(ctx)
		}
	}
}

func (p *Plugin) wakeReporter() {
	if p.reportWake == nil {
		return
	}
	select {
	case p.reportWake <- struct{}{}:
	default:
	}
}

func (p *Plugin) flushSegments(ctx context.Context) {
	p.reportMu.Lock()
	if len(p.segments) == 0 {
		p.reportMu.Unlock()
		return
	}
	segments := append([]skywalkingSegment(nil), p.segments...)
	p.segments = p.segments[:0]
	client := p.client
	p.reportMu.Unlock()

	if client == nil {
		p.requeueSegments(segments)
		return
	}
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetBody(segments).
		Post(p.endpointURL())
	if err != nil {
		logger.Errorf("failed to report SkyWalking segment to %s: %s", p.endpointURL(), err)
		p.requeueSegments(segments)
		return
	}
	if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
		logger.Errorf("SkyWalking endpoint returned status code [%d], body [%s]", resp.StatusCode(), resp.String())
		p.requeueSegments(segments)
	}
}

// requeueSegments returns failed segments to the bounded window and wakes the
// reporter for a future retry; segments that no longer fit are dropped.
func (p *Plugin) requeueSegments(segments []skywalkingSegment) {
	p.reportMu.Lock()
	if p.stopped {
		p.dropped += len(segments)
		p.reportMu.Unlock()
		return
	}
	space := maxPendingSkyWalkingSegments - len(p.segments)
	kept := min(len(segments), space)
	if kept > 0 {
		p.segments = append(segments[:kept], p.segments...)
	}
	if dropped := len(segments) - kept; dropped > 0 {
		p.dropped += dropped
	}
	shouldWake := len(p.segments) > 0
	p.reportMu.Unlock()
	if shouldWake {
		p.wakeReporter()
	}
}

func (p *Plugin) endpointURL() string {
	return strings.TrimRight(p.config.EndpointAddr, "/") + "/v3/segments"
}

func (p *Plugin) serviceInstanceName() string {
	if p.config.ServiceInstanceName != "$hostname" {
		return p.config.ServiceInstanceName
	}
	if hostname := base.Hostname(); hostname != "" {
		return hostname
	}
	return "$hostname"
}
