package zipkin

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client    *resty.Client
	processor *logger_batch.Processor

	clientRelease func()

	sampleRandom func() (float64, error)

	reportTimeout     time.Duration
	maxPendingEntries int
}

const (
	priority = 12011
	name     = "zipkin"

	defaultReportTimeout     = 5 * time.Second
	defaultMaxPendingEntries = 100
)

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint": {
      "type": "string"
    },
    "sample_ratio": {
      "type": "number",
      "minimum": 0.00001,
      "maximum": 1
    },
    "service_name": {
      "type": "string",
      "default": "APISIX"
    },
    "server_addr": {
      "type": "string"
    },
    "span_version": {
      "enum": [2],
      "default": 2
    }
  },
  "required": ["endpoint", "sample_ratio"]
}
`

var hexIDPattern = regexp.MustCompile(`^[0-9a-fA-F]+$`)

type Config struct {
	Endpoint    string  `json:"endpoint"`
	SampleRatio float64 `json:"sample_ratio"`
	ServiceName string  `json:"service_name,omitempty"`
	ServerAddr  string  `json:"server_addr,omitempty"`
	SpanVersion int     `json:"span_version,omitempty"`
}

type b3Context struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Sampled      string
}

type spanStateContextKey struct{}

type spanState struct {
	context b3Context
	started time.Time
	once    sync.Once
}

type zipkinEndpoint struct {
	ServiceName string `json:"serviceName"`
	IPv4        string `json:"ipv4,omitempty"`
	IPv6        string `json:"ipv6,omitempty"`
	Port        int    `json:"port,omitempty"`
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.SpanVersion != 0 && p.config.SpanVersion != 2 {
		return fmt.Errorf("zipkin span_version %d is unsupported; only v2 is emitted", p.config.SpanVersion)
	}
	if p.config.ServiceName == "" {
		p.config.ServiceName = "APISIX"
	}
	if p.config.SpanVersion == 0 {
		p.config.SpanVersion = 2
	}
	if p.sampleRandom == nil {
		p.sampleRandom = func() (float64, error) { return randomUnit(rand.Reader) }
	}
	if p.reportTimeout <= 0 {
		p.reportTimeout = defaultReportTimeout
	}
	if p.maxPendingEntries <= 0 {
		p.maxPendingEntries = defaultMaxPendingEntries
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.Endpoint)
	client := resty.New()
	client.SetTimeout(p.reportTimeout)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: p.reportTimeout}).DialContext
	client.SetTransport(transport)
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

	processor, err := logger_batch.NewWithContext(logger_batch.Config{
		Tasks:             p.TaskOwner(),
		Name:              "zipkin span reporter",
		PluginID:          name,
		BatchMaxSize:      1,
		MaxRetryCount:     0,
		RetryDelay:        logger_batch.DefaultRetryDelay,
		BufferDuration:    logger_batch.DefaultBufferDuration,
		InactiveTimeout:   logger_batch.DefaultInactiveTimeout,
		MaxPendingEntries: p.maxPendingEntries,
	}, p.deliverSpans)
	if err != nil {
		p.clientRelease()
		p.clientRelease = nil
		p.client = nil
		return err
	}
	p.processor = processor

	return nil
}

func (p *Plugin) QuiesceGenerationTasks() { p.Stop() }

func (p *Plugin) Stop() {
	cleanup := func() {
		if p.clientRelease != nil {
			p.clientRelease()
			p.clientRelease = nil
		}
	}
	if p.processor != nil {
		p.processor.StopWithCleanup(cleanup)
		return
	}
	cleanup()
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// Production route assembly invokes RunRequestPhase. Preserve this
		// direct Handler only for package compatibility and never duplicate a
		// lifecycle-owned span.
		if apisixctx.GetRequestLifecycle(r) != nil || r.Context().Value(spanStateContextKey{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx, err := extractB3(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if ctx.TraceID == "" {
			traceID, err := randomHex(rand.Reader, 16)
			if err != nil {
				http.Error(w, "failed to generate trace id", http.StatusInternalServerError)
				return
			}
			ctx.TraceID = traceID
		}
		if ctx.Sampled == "" {
			sample, err := p.shouldSample()
			if err != nil {
				http.Error(w, "failed to generate sampling decision", http.StatusInternalServerError)
				return
			}
			if sample {
				ctx.Sampled = "1"
			} else {
				ctx.Sampled = "0"
			}
		}
		if ctx.SpanID != "" {
			ctx.ParentSpanID = ctx.SpanID
		}
		spanID, err := randomHex(rand.Reader, 8)
		if err != nil {
			http.Error(w, "failed to generate span id", http.StatusInternalServerError)
			return
		}
		ctx.SpanID = spanID
		injectB3(r, ctx)

		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}

		if ctx.Sampled == "1" {
			if !p.processor.Push(p.buildSpan(ctx, r, recorder.status, start, time.Since(start))) {
				logger.Errorf("failed to enqueue zipkin span to %s", p.config.Endpoint)
			}
		}
	}
	return http.HandlerFunc(fn)
}

// RunRequestPhase starts Zipkin propagation at the inherited rewrite stage.
// Sampled requests register one dynamic lifecycle finalizer; sampled=0
// requests continue without exporter ownership.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	if r == nil {
		return base.ContinueRequest(r)
	}
	if _, exists := r.Context().Value(spanStateContextKey{}).(*spanState); exists {
		return base.ContinueRequest(r)
	}
	lifecycle := apisixctx.GetRequestLifecycle(r)
	if lifecycle == nil {
		r, lifecycle = apisixctx.EnsureRequestLifecycle(r, time.Now())
	}
	traceContext, err := extractB3(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
	}
	if traceContext.TraceID == "" {
		traceContext.TraceID, err = randomHex(rand.Reader, 16)
		if err != nil {
			http.Error(w, "failed to generate trace id", http.StatusInternalServerError)
			return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
		}
	}
	if traceContext.Sampled == "" {
		sample, sampleErr := p.shouldSample()
		if sampleErr != nil {
			http.Error(w, "failed to generate sampling decision", http.StatusInternalServerError)
			return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
		}
		if sample {
			traceContext.Sampled = "1"
		} else {
			traceContext.Sampled = "0"
		}
	}
	if traceContext.SpanID != "" {
		traceContext.ParentSpanID = traceContext.SpanID
	}
	traceContext.SpanID, err = randomHex(rand.Reader, 8)
	if err != nil {
		http.Error(w, "failed to generate span id", http.StatusInternalServerError)
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
	}
	injectB3(r, traceContext)
	state := &spanState{context: traceContext, started: time.Now()}
	r = r.WithContext(context.WithValue(r.Context(), spanStateContextKey{}, state))
	if traceContext.Sampled != "1" {
		return base.ContinueRequest(r)
	}
	if !lifecycle.AddFinalizer(name, func() error {
		return p.finishSpan(state, lifecycle, r)
	}) {
		return base.ContinueRequest(r)
	}
	return base.ContinueRequest(r)
}

func (p *Plugin) finishSpan(state *spanState, lifecycle *apisixctx.RequestLifecycle, fallback *http.Request) error {
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
		if !p.processor.Push(p.buildSpanWithSource(
			state.context,
			request,
			status,
			state.started,
			duration,
			lifecycle.ResponseSource(),
			outcome.Kind,
		)) {
			logger.Errorf("failed to enqueue zipkin span to %s", p.config.Endpoint)
		}
	})
	return nil
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

func (p *Plugin) buildSpan(
	ctx b3Context,
	r *http.Request,
	status int,
	start time.Time,
	duration time.Duration,
) map[string]any {
	return p.buildSpanWithSource(ctx, r, status, start, duration, apisixctx.ResponseSourceUnknown)
}

func (p *Plugin) buildSpanWithSource(
	ctx b3Context,
	r *http.Request,
	status int,
	start time.Time,
	duration time.Duration,
	source apisixctx.ResponseSource,
	outcomes ...apisixctx.RequestOutcomeKind,
) map[string]any {
	serverAddr := p.config.ServerAddr
	if serverAddr == "" {
		serverAddr = requestServerAddr(r)
	}
	if serverAddr == "" {
		serverAddr = localIPv4()
	}

	tags := map[string]string{
		"component":        "apisix",
		"http.method":      r.Method,
		"http.url":         r.URL.RequestURI(),
		"http.status_code": strconv.Itoa(status),
	}
	var remoteEndpoint *zipkinEndpoint
	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteEndpoint = &zipkinEndpoint{}
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			remoteEndpoint.IPv6 = host
		} else {
			remoteEndpoint.IPv4 = host
		}
		remoteEndpoint.Port, _ = strconv.Atoi(port)
	}
	if status >= http.StatusInternalServerError {
		tags["error"] = "true"
	}
	if source != apisixctx.ResponseSourceUnknown {
		tags["apisix.response_source"] = string(source)
	}
	correlation := apisixlog.CaptureRequestCorrelation(r)
	for key, value := range map[string]string{
		"apisix.request_id":         correlation.RequestID,
		"apisix.node_id":            correlation.NodeID,
		"apisix.retry_count":        correlation.RetryCount,
		"http.upstream_status_code": correlation.UpstreamStatus,
	} {
		if value != "" {
			tags[key] = value
		}
	}
	if len(outcomes) > 0 && outcomes[0] != "" {
		tags["apisix.outcome"] = string(outcomes[0])
	}

	localEndpoint := map[string]any{"serviceName": p.config.ServiceName}
	if serverAddr != "" {
		localEndpoint["ipv4"] = serverAddr
	}
	if port := requestServerPort(r); port != 0 {
		localEndpoint["port"] = port
	}

	span := map[string]any{
		"traceId":       ctx.TraceID,
		"name":          "apisix.request",
		"id":            ctx.SpanID,
		"kind":          "SERVER",
		"timestamp":     start.UnixNano() / int64(time.Microsecond),
		"duration":      duration.Nanoseconds() / int64(time.Microsecond),
		"localEndpoint": localEndpoint,
		"tags":          tags,
	}
	if ctx.ParentSpanID != "" {
		span["parentId"] = ctx.ParentSpanID
	}
	if remoteEndpoint != nil {
		endpoint := map[string]any{}
		if remoteEndpoint.IPv6 != "" {
			endpoint["ipv6"] = remoteEndpoint.IPv6
		} else if remoteEndpoint.IPv4 != "" {
			endpoint["ipv4"] = remoteEndpoint.IPv4
		}
		if remoteEndpoint.Port != 0 {
			endpoint["port"] = remoteEndpoint.Port
		}
		span["remoteEndpoint"] = endpoint
	}

	return span
}

// deliverSpans posts enqueued spans to the Zipkin collector. Delivery
// failures are returned to the batch processor for accounting and never
// alter the proxied response.
func (p *Plugin) deliverSpans(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	resp, err := p.client.R().
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetBody(entries).
		Post(p.config.Endpoint)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return 0, fmt.Errorf("zipkin endpoint returned status code [%d]", resp.StatusCode())
	}
	return 0, nil
}

func randomHex(reader io.Reader, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func randomUnit(reader io.Reader) (float64, error) {
	var raw [8]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return 0, fmt.Errorf("read random bytes: %w", err)
	}
	return float64(binary.BigEndian.Uint64(raw[:])>>11) / (1 << 53), nil
}

func requestServerPort(r *http.Request) int {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if local == nil {
		return 0
	}
	_, port, err := net.SplitHostPort(local.String())
	if err != nil {
		return 0
	}
	value, _ := strconv.Atoi(port)
	return value
}

func requestServerAddr(r *http.Request) string {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if local == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(local.String())
	if err != nil {
		return ""
	}
	return host
}

func localIPv4() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return addr.IP.To4().String()
}
