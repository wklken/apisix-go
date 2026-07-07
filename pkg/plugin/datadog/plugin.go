package datadog

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	metadata Metadata
}

const (
	priority = 495
	name     = "datadog"
)

const schema = `
{
  "type": "object",
  "properties": {
    "prefer_name": {
      "type": "boolean",
      "default": true
    },
    "include_path": {
      "type": "boolean",
      "default": false
    },
    "include_method": {
      "type": "boolean",
      "default": false
    },
    "constant_tags": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1,
        "maxLength": 200
      },
      "default": []
    }
  }
}
`

type Config struct {
	PreferName    bool     `json:"prefer_name,omitempty"`
	IncludePath   bool     `json:"include_path,omitempty"`
	IncludeMethod bool     `json:"include_method,omitempty"`
	ConstantTags  []string `json:"constant_tags,omitempty"`
	preferNameSet bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type configJSON struct {
		PreferName    *bool    `json:"prefer_name,omitempty"`
		IncludePath   bool     `json:"include_path,omitempty"`
		IncludeMethod bool     `json:"include_method,omitempty"`
		ConstantTags  []string `json:"constant_tags,omitempty"`
	}

	var decoded configJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	if decoded.PreferName != nil {
		c.PreferName = *decoded.PreferName
		c.preferNameSet = true
	}
	c.IncludePath = decoded.IncludePath
	c.IncludeMethod = decoded.IncludeMethod
	c.ConstantTags = decoded.ConstantTags
	return nil
}

type Metadata struct {
	Host         string   `json:"host,omitempty"`
	Port         int      `json:"port,omitempty"`
	Namespace    string   `json:"namespace,omitempty"`
	ConstantTags []string `json:"constant_tags,omitempty"`
}

type metricEntry struct {
	LatencyMS       int64
	UpstreamLatency int64
	ApisixLatency   int64
	IngressSize     int64
	EgressSize      int64
	Status          int
	RouteID         string
	RouteName       string
	ServiceID       string
	ServiceName     string
	ConsumerName    string
	BalancerIP      string
	Path            string
	Method          string
	Scheme          string
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += int64(n)
	return n, err
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if !p.config.preferNameSet {
		p.config.PreferName = true
	}
	p.metadata = loadMetadata()
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}

		latency := time.Since(start).Milliseconds()
		upstreamLatency := requestInt64Var(r, "$upstream_latency")
		p.Send(metricEntry{
			LatencyMS:       latency,
			UpstreamLatency: upstreamLatency,
			ApisixLatency:   apisixLatency(latency, upstreamLatency),
			IngressSize:     requestSize(r),
			EgressSize:      recorder.bytes,
			Status:          recorder.status,
			RouteID:         apisixStringVar(r, "$route_id"),
			RouteName:       apisixStringVar(r, "$route_name"),
			ServiceID:       apisixStringVar(r, "$service_id"),
			ServiceName:     apisixStringVar(r, "$service_name"),
			ConsumerName:    consumerName(r),
			BalancerIP:      apisixStringVar(r, "$balancer_ip"),
			Path:            matchedPath(r),
			Method:          r.Method,
			Scheme:          requestScheme(r),
		})
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(entry metricEntry) {
	addr := net.JoinHostPort(p.metadata.Host, fmt.Sprint(p.metadata.Port))
	conn, err := net.Dial("udp", addr)
	if err != nil {
		logger.Errorf("failed to connect to DogStatsD endpoint %s: %s", addr, err)
		return
	}
	defer conn.Close()

	for _, line := range p.metricLines(entry) {
		if _, err := conn.Write([]byte(line)); err != nil {
			logger.Errorf("failed to send DogStatsD metric %q: %s", line, err)
		}
	}
}

func (p *Plugin) metricLines(entry metricEntry) []string {
	tags := p.generateTags(entry)
	lines := []string{
		p.metricLine("request.counter", "1", "c", tags),
		p.metricLine("request.latency", strconv.FormatInt(entry.LatencyMS, 10), "h", tags),
		p.metricLine("apisix.latency", strconv.FormatInt(entry.ApisixLatency, 10), "h", tags),
		p.metricLine("ingress.size", strconv.FormatInt(entry.IngressSize, 10), "ms", tags),
		p.metricLine("egress.size", strconv.FormatInt(entry.EgressSize, 10), "ms", tags),
	}
	if entry.UpstreamLatency > 0 {
		lines = append(lines, p.metricLine("upstream.latency", strconv.FormatInt(entry.UpstreamLatency, 10), "h", tags))
	}
	return lines
}

func (p *Plugin) metricLine(metricName string, value string, metricType string, tags []string) string {
	prefix := p.metadata.Namespace
	if prefix != "" {
		prefix += "."
	}
	line := prefix + metricName + ":" + value + "|" + metricType
	if len(tags) > 0 {
		line += "|#" + strings.Join(tags, ",")
	}
	return line
}

func (p *Plugin) generateTags(entry metricEntry) []string {
	tags := make([]string, 0, len(p.metadata.ConstantTags)+len(p.config.ConstantTags)+6)
	tags = append(tags, p.metadata.ConstantTags...)
	tags = append(tags, p.config.ConstantTags...)
	if p.config.IncludePath && entry.Path != "" {
		tags = append(tags, "path:"+entry.Path)
	}
	if p.config.IncludeMethod && entry.Method != "" {
		tags = append(tags, "method:"+entry.Method)
	}
	if route := resourceTag(entry.RouteID, entry.RouteName, p.config.PreferName); route != "" {
		tags = append(tags, "route_name:"+route)
	}
	if service := resourceTag(entry.ServiceID, entry.ServiceName, p.config.PreferName); service != "" {
		tags = append(tags, "service_name:"+service)
	}
	if entry.ConsumerName != "" {
		tags = append(tags, "consumer:"+entry.ConsumerName)
	}
	if entry.BalancerIP != "" {
		tags = append(tags, "balancer_ip:"+entry.BalancerIP)
	}
	if entry.Status > 0 {
		status := strconv.Itoa(entry.Status)
		tags = append(tags, "response_status:"+status)
		tags = append(tags, "response_status_class:"+status[:1]+"xx")
	}
	if entry.Scheme != "" {
		tags = append(tags, "scheme:"+entry.Scheme)
	}
	return tags
}

func resourceTag(id string, name string, preferName bool) string {
	if preferName && name != "" {
		return name
	}
	if id != "" {
		return id
	}
	return name
}

func apisixStringVar(r *http.Request, key string) string {
	value := ctx.GetApisixVar(r, key)
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func consumerName(r *http.Request) string {
	if name := apisixStringVar(r, "$consumer_name"); name != "" {
		return name
	}
	consumer, ok := ctx.GetApisixVar(r, "$consumer").(resource.Consumer)
	if !ok {
		return ""
	}
	return consumer.Username
}

func matchedPath(r *http.Request) string {
	if path := apisixStringVar(r, "$matched_uri"); path != "" {
		return path
	}
	return r.URL.Path
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

func apisixLatency(total int64, upstream int64) int64 {
	if upstream <= 0 {
		return total
	}
	if total <= upstream {
		return 0
	}
	return total - upstream
}

func requestSize(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}

func requestScheme(r *http.Request) string {
	if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
		return scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func loadMetadata() (metadata Metadata) {
	metadata = Metadata{
		Host:         "127.0.0.1",
		Port:         8125,
		Namespace:    "apisix",
		ConstantTags: []string{"source:apisix"},
	}
	defer func() {
		if recover() != nil {
			metadata = Metadata{
				Host:         "127.0.0.1",
				Port:         8125,
				Namespace:    "apisix",
				ConstantTags: []string{"source:apisix"},
			}
		}
	}()

	var configured Metadata
	if err := store.GetPluginMetadata(name, &configured); err != nil {
		return metadata
	}
	if configured.Host != "" {
		metadata.Host = configured.Host
	}
	if configured.Port != 0 {
		metadata.Port = configured.Port
	}
	if configured.Namespace != "" {
		metadata.Namespace = configured.Namespace
	}
	if len(configured.ConstantTags) > 0 {
		metadata.ConstantTags = configured.ConstantTags
	}
	return metadata
}
