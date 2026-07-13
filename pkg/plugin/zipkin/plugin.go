package zipkin

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *resty.Client

	sampleRandom func() float64
}

const (
	priority = 12011
	name     = "zipkin"
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
      "enum": [1, 2],
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

type zipkinSpan struct {
	TraceID        string            `json:"traceId"`
	Name           string            `json:"name"`
	ParentID       string            `json:"parentId,omitempty"`
	ID             string            `json:"id"`
	Kind           string            `json:"kind,omitempty"`
	Timestamp      int64             `json:"timestamp"`
	Duration       int64             `json:"duration"`
	LocalEndpoint  zipkinEndpoint    `json:"localEndpoint"`
	RemoteEndpoint *zipkinEndpoint   `json:"remoteEndpoint,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
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
	if p.config.ServiceName == "" {
		p.config.ServiceName = "APISIX"
	}
	if p.config.SpanVersion == 0 {
		p.config.SpanVersion = 2
	}
	if p.sampleRandom == nil {
		p.sampleRandom = randomUnit
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.Endpoint)
	client := resty.New()
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx, err := extractB3(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if ctx.TraceID == "" {
			ctx.TraceID = randomHex(16)
		}
		if ctx.Sampled == "" {
			if p.shouldSample() {
				ctx.Sampled = "1"
			} else {
				ctx.Sampled = "0"
			}
		}
		if ctx.SpanID != "" {
			ctx.ParentSpanID = ctx.SpanID
		}
		ctx.SpanID = randomHex(8)
		injectB3(r, ctx)

		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}

		if ctx.Sampled == "1" {
			p.reportSpan(p.buildSpan(ctx, r, recorder.status, start, time.Since(start)))
		}
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) shouldSample() bool {
	return p.config.SampleRatio >= 1 || p.sampleRandom() < p.config.SampleRatio
}

func extractB3(r *http.Request) (b3Context, error) {
	if b3 := r.Header.Get("b3"); b3 != "" {
		return parseSingleB3(b3)
	}

	ctx := b3Context{
		TraceID:      strings.ToLower(r.Header.Get("x-b3-traceid")),
		SpanID:       strings.ToLower(r.Header.Get("x-b3-spanid")),
		ParentSpanID: strings.ToLower(r.Header.Get("x-b3-parentspanid")),
		Sampled:      normalizeSampled(r.Header.Get("x-b3-sampled")),
	}
	if r.Header.Get("x-b3-flags") == "1" {
		ctx.Sampled = "1"
	}
	if err := validateB3IDs(ctx); err != nil {
		return b3Context{}, err
	}
	return ctx, nil
}

func parseSingleB3(header string) (b3Context, error) {
	if header == "0" || header == "1" || header == "d" {
		sampled := header
		if sampled == "d" {
			sampled = "1"
		}
		return b3Context{Sampled: sampled}, nil
	}

	parts := strings.Split(header, "-")
	if len(parts) < 2 {
		return b3Context{}, errors.New("invalid b3 header: missing span id")
	}
	if len(parts) > 4 {
		return b3Context{}, errors.New("invalid b3 header: too many fields")
	}

	ctx := b3Context{
		TraceID:      strings.ToLower(parts[0]),
		SpanID:       strings.ToLower(parts[1]),
		ParentSpanID: "",
	}
	if len(parts) >= 3 {
		ctx.Sampled = normalizeSampled(parts[2])
	}
	if len(parts) == 4 {
		ctx.ParentSpanID = strings.ToLower(parts[3])
	}
	if err := validateB3IDs(ctx); err != nil {
		return b3Context{}, err
	}
	return ctx, nil
}

func normalizeSampled(value string) string {
	switch strings.ToLower(value) {
	case "1", "true", "d":
		return "1"
	case "0", "false":
		return "0"
	default:
		return ""
	}
}

func validateB3IDs(ctx b3Context) error {
	if ctx.TraceID != "" && !validTraceID(ctx.TraceID) {
		return errors.New("invalid b3 header: invalid trace id")
	}
	if ctx.SpanID != "" && !validSpanID(ctx.SpanID) {
		return errors.New("invalid b3 header: invalid span id")
	}
	if ctx.ParentSpanID != "" && !validSpanID(ctx.ParentSpanID) {
		return errors.New("invalid b3 header: invalid parent span id")
	}
	return nil
}

func validTraceID(value string) bool {
	return (len(value) == 16 || len(value) == 32) && hexIDPattern.MatchString(value)
}

func validSpanID(value string) bool {
	return len(value) == 16 && hexIDPattern.MatchString(value)
}

func injectB3(r *http.Request, ctx b3Context) {
	r.Header.Set("x-b3-traceid", ctx.TraceID)
	r.Header.Set("x-b3-spanid", ctx.SpanID)
	r.Header.Set("x-b3-sampled", ctx.Sampled)
	if ctx.ParentSpanID != "" {
		r.Header.Set("x-b3-parentspanid", ctx.ParentSpanID)
	} else {
		r.Header.Del("x-b3-parentspanid")
	}
	r.Header.Del("b3")
}

func (p *Plugin) buildSpan(
	ctx b3Context,
	r *http.Request,
	status int,
	start time.Time,
	duration time.Duration,
) zipkinSpan {
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

	return zipkinSpan{
		TraceID:   ctx.TraceID,
		Name:      "apisix.request",
		ParentID:  ctx.ParentSpanID,
		ID:        ctx.SpanID,
		Kind:      "SERVER",
		Timestamp: start.UnixNano() / int64(time.Microsecond),
		Duration:  duration.Nanoseconds() / int64(time.Microsecond),
		LocalEndpoint: zipkinEndpoint{
			ServiceName: p.config.ServiceName,
			IPv4:        serverAddr,
			Port:        requestServerPort(r),
		},
		RemoteEndpoint: remoteEndpoint,
		Tags:           tags,
	}
}

func (p *Plugin) reportSpan(span zipkinSpan) {
	resp, err := p.client.R().
		SetHeader("Content-Type", "application/json").
		SetBody([]zipkinSpan{span}).
		Post(p.config.Endpoint)
	if err != nil {
		logger.Errorf("failed to report zipkin span to %s: %s", p.config.Endpoint, err)
		return
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		logger.Errorf("zipkin endpoint returned status code [%d], body [%s]", resp.StatusCode(), resp.String())
	}
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("read random bytes: %s", err))
	}
	return hex.EncodeToString(buf)
}

func randomUnit() float64 {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("read random bytes: %s", err))
	}
	return float64(binary.BigEndian.Uint64(raw[:])>>11) / (1 << 53)
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
