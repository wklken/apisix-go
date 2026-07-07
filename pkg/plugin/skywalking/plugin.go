package skywalking

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *resty.Client
}

const (
	priority = 12010
	name     = "skywalking"

	componentIDAPISIX = 6002
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
	TraceID          string                `json:"traceId"`
	TraceSegmentID   string                `json:"traceSegmentId"`
	Service          string                `json:"service"`
	ServiceInstance  string                `json:"serviceInstance"`
	Spans            []skywalkingSpan      `json:"spans"`
	SegmentReference *skywalkingSegmentRef `json:"segmentReference,omitempty"`
}

type skywalkingSegmentRef struct {
	TraceID               string `json:"traceId"`
	ParentTraceSegmentID  string `json:"parentTraceSegmentId"`
	ParentSpanID          int    `json:"parentSpanId"`
	ParentService         string `json:"parentService"`
	ParentServiceInstance string `json:"parentServiceInstance"`
	ParentEndpoint        string `json:"parentEndpoint"`
	AddressUsedAtClient   string `json:"addressUsedAtClient"`
}

type skywalkingSpan struct {
	SpanID        int                 `json:"spanId"`
	ParentSpanID  int                 `json:"parentSpanId"`
	OperationName string              `json:"operationName"`
	StartTime     int64               `json:"startTime"`
	EndTime       int64               `json:"endTime"`
	SpanType      string              `json:"spanType"`
	SpanLayer     string              `json:"spanLayer"`
	ComponentID   int                 `json:"componentId"`
	IsError       bool                `json:"isError"`
	Tags          map[string]string   `json:"tags,omitempty"`
	Logs          []map[string]string `json:"logs,omitempty"`
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

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

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	p.loadPluginAttr()
	p.applyDefaults()

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.EndpointAddr)
	client := resty.New()
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if !p.shouldSample() {
			next.ServeHTTP(w, r)
			return
		}

		ctx, _ := parseSW8(r.Header.Get("sw8"))
		if ctx.TraceID == "" {
			ctx.TraceID = randomID(16)
		}
		if ctx.TraceSegmentID == "" {
			ctx.TraceSegmentID = randomID(16)
		}
		ctx.SpanID = 0
		r.Header.Set("sw8", ctx.header(p.config.ServiceName, p.serviceInstanceName(), r.URL.Path))

		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}

		p.reportSegment(p.buildSegment(ctx, r, recorder.status, start, time.Since(start)))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) loadPluginAttr() {
	if config.GlobalConfig == nil || config.GlobalConfig.PluginAttr == nil {
		return
	}
	attr, ok := config.GlobalConfig.PluginAttr[name]
	if !ok {
		return
	}
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

func (p *Plugin) shouldSample() bool {
	return p.config.SampleRatio >= 1
}

func (p *Plugin) buildSegment(
	ctx sw8Context,
	r *http.Request,
	status int,
	start time.Time,
	duration time.Duration,
) skywalkingSegment {
	end := start.Add(duration)
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
		Tags: map[string]string{
			"http.method":      r.Method,
			"http.url":         r.URL.RequestURI(),
			"http.status_code": fmt.Sprint(status),
		},
	}
	segment := skywalkingSegment{
		TraceID:         ctx.TraceID,
		TraceSegmentID:  ctx.TraceSegmentID,
		Service:         p.config.ServiceName,
		ServiceInstance: p.serviceInstanceName(),
		Spans:           []skywalkingSpan{span},
	}
	if ctx.ParentTraceSegmentID != "" {
		segment.SegmentReference = &skywalkingSegmentRef{
			TraceID:               ctx.TraceID,
			ParentTraceSegmentID:  ctx.ParentTraceSegmentID,
			ParentSpanID:          ctx.ParentSpanID,
			ParentService:         ctx.ParentService,
			ParentServiceInstance: ctx.ParentInstance,
			ParentEndpoint:        ctx.ParentEndpoint,
			AddressUsedAtClient:   ctx.AddressUsedAtClient,
		}
	}
	return segment
}

func (p *Plugin) reportSegment(segment skywalkingSegment) {
	resp, err := p.client.R().
		SetHeader("Content-Type", "application/json").
		SetBody([]skywalkingSegment{segment}).
		Post(p.endpointURL())
	if err != nil {
		logger.Errorf("failed to report SkyWalking segment to %s: %s", p.endpointURL(), err)
		return
	}
	if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
		logger.Errorf("SkyWalking endpoint returned status code [%d], body [%s]", resp.StatusCode(), resp.String())
	}
}

func (p *Plugin) endpointURL() string {
	return strings.TrimRight(p.config.EndpointAddr, "/") + "/v3/segments"
}

func (p *Plugin) serviceInstanceName() string {
	if p.config.ServiceInstanceName != "$hostname" {
		return p.config.ServiceInstanceName
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "$hostname"
	}
	return hostname
}

func parseSW8(header string) (sw8Context, bool) {
	if header == "" {
		return sw8Context{}, false
	}
	parts := strings.Split(header, "-")
	if len(parts) != 8 {
		return sw8Context{}, false
	}

	traceID, err := decodeBase64URL(parts[1])
	if err != nil {
		return sw8Context{}, false
	}
	segmentID, err := decodeBase64URL(parts[2])
	if err != nil {
		return sw8Context{}, false
	}
	spanID := 0
	if _, err := fmt.Sscanf(parts[3], "%d", &spanID); err != nil {
		return sw8Context{}, false
	}
	parentService, _ := decodeBase64URL(parts[4])
	parentInstance, _ := decodeBase64URL(parts[5])
	parentEndpoint, _ := decodeBase64URL(parts[6])

	return sw8Context{
		TraceID:              traceID,
		ParentTraceSegmentID: segmentID,
		ParentSpanID:         spanID,
		ParentService:        parentService,
		ParentInstance:       parentInstance,
		ParentEndpoint:       parentEndpoint,
		AddressUsedAtClient:  parts[7],
	}, true
}

func (ctx sw8Context) header(service, instance, endpoint string) string {
	return strings.Join([]string{
		"1",
		encodeBase64URL(ctx.TraceID),
		encodeBase64URL(ctx.TraceSegmentID),
		fmt.Sprint(ctx.SpanID),
		encodeBase64URL(service),
		encodeBase64URL(instance),
		encodeBase64URL(endpoint),
		encodeBase64URL("apisix-go"),
	}, "-")
}

func decodeBase64URL(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return string(decoded), nil
	}
	decoded, err = base64.URLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func encodeBase64URL(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func randomID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("read random bytes: %s", err))
	}
	return hex.EncodeToString(buf)
}

func intFromAttr(attr map[string]interface{}, key string) int {
	value, ok := attr[key]
	if !ok {
		return 0
	}
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case uint64:
		return int(v)
	default:
		return 0
	}
}
