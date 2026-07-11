package datadog

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	metadata Metadata

	BatchProcessor *logger_batch.Processor
	RouteID        string
	ServerAddr     string
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
    },
    "name": {
      "type": "string",
      "default": "datadog"
    },
    "batch_max_size": {
      "type": "integer",
      "minimum": 1,
      "default": 1000
    },
    "max_retry_count": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    },
    "retry_delay": {
      "type": "integer",
      "minimum": 0,
      "default": 1
    },
    "buffer_duration": {
      "type": "integer",
      "minimum": 1,
      "default": 60
    },
    "inactive_timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    }
  }
}
`

type Config struct {
	PreferName      bool     `json:"prefer_name,omitempty"`
	IncludePath     bool     `json:"include_path,omitempty"`
	IncludeMethod   bool     `json:"include_method,omitempty"`
	ConstantTags    []string `json:"constant_tags,omitempty"`
	BatchName       string   `json:"name,omitempty"`
	BatchMaxSize    int      `json:"batch_max_size,omitempty"`
	MaxRetryCount   int      `json:"max_retry_count,omitempty"`
	RetryDelay      int      `json:"retry_delay,omitempty"`
	BufferDuration  int      `json:"buffer_duration,omitempty"`
	InactiveTimeout int      `json:"inactive_timeout,omitempty"`
	preferNameSet   bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type configJSON struct {
		PreferName      *bool    `json:"prefer_name,omitempty"`
		IncludePath     bool     `json:"include_path,omitempty"`
		IncludeMethod   bool     `json:"include_method,omitempty"`
		ConstantTags    []string `json:"constant_tags,omitempty"`
		BatchName       string   `json:"name,omitempty"`
		BatchMaxSize    int      `json:"batch_max_size,omitempty"`
		MaxRetryCount   int      `json:"max_retry_count,omitempty"`
		RetryDelay      int      `json:"retry_delay,omitempty"`
		BufferDuration  int      `json:"buffer_duration,omitempty"`
		InactiveTimeout int      `json:"inactive_timeout,omitempty"`
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
	c.BatchName = decoded.BatchName
	c.BatchMaxSize = decoded.BatchMaxSize
	c.MaxRetryCount = decoded.MaxRetryCount
	c.RetryDelay = decoded.RetryDelay
	c.BufferDuration = decoded.BufferDuration
	c.InactiveTimeout = decoded.InactiveTimeout
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
	if p.config.BatchName == "" {
		p.config.BatchName = name
	}
	if p.config.BatchMaxSize == 0 {
		p.config.BatchMaxSize = logger_batch.DefaultBatchMaxSize
	}
	if p.config.RetryDelay == 0 {
		p.config.RetryDelay = int(logger_batch.DefaultRetryDelay / time.Second)
	}
	if p.config.BufferDuration == 0 {
		p.config.BufferDuration = int(logger_batch.DefaultBufferDuration / time.Second)
	}
	if p.config.InactiveTimeout == 0 {
		p.config.InactiveTimeout = int(logger_batch.DefaultInactiveTimeout / time.Second)
	}
	p.metadata = loadMetadata()
	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:            p.config.BatchName,
		BatchMaxSize:    p.config.BatchMaxSize,
		MaxRetryCount:   p.config.MaxRetryCount,
		RetryDelay:      time.Duration(p.config.RetryDelay) * time.Second,
		BufferDuration:  time.Duration(p.config.BufferDuration) * time.Second,
		InactiveTimeout: time.Duration(p.config.InactiveTimeout) * time.Second,
		RouteID:         p.RouteID,
		ServerAddr:      p.ServerAddr,
	}, p.deliver)
	return nil
}

func (p *Plugin) SetRouteContext(routeID string, serverAddr string) {
	p.RouteID = routeID
	p.ServerAddr = serverAddr
}

func (p *Plugin) Stop() {
	if p.BatchProcessor != nil {
		p.BatchProcessor.Stop()
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		captured := httpsnoop.CaptureMetrics(next, w, r)
		upstreamLatency := requestInt64Var(r, "$upstream_latency")
		entry := metricEntry{
			LatencyMS:       captured.Duration.Milliseconds(),
			UpstreamLatency: upstreamLatency,
			ApisixLatency:   apisixLatency(captured.Duration.Milliseconds(), upstreamLatency),
			IngressSize:     requestSize(r),
			EgressSize:      captured.Written,
			Status:          captured.Code,
			RouteID:         apisixStringVar(r, "$route_id"),
			RouteName:       apisixStringVar(r, "$route_name"),
			ServiceID:       apisixStringVar(r, "$service_id"),
			ServiceName:     apisixStringVar(r, "$service_name"),
			ConsumerName:    consumerName(r),
			BalancerIP:      apisixStringVar(r, "$balancer_ip"),
			Path:            matchedPath(r),
			Method:          r.Method,
			Scheme:          requestScheme(r),
		}
		p.BatchProcessor.Push(map[string]any{"entry": entry})
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(entry metricEntry) {
	if err := p.send(entry); err != nil {
		logger.Errorf("failed to send DogStatsD metrics: %s", err)
	}
}

func (p *Plugin) send(entry metricEntry) error {
	addr := net.JoinHostPort(p.metadata.Host, fmt.Sprint(p.metadata.Port))
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return fmt.Errorf("connect to DogStatsD endpoint %s: %w", addr, err)
	}
	defer conn.Close()

	for _, line := range p.metricLines(entry) {
		if _, err := conn.Write([]byte(line)); err != nil {
			return fmt.Errorf("send DogStatsD metric %q: %w", line, err)
		}
	}
	return nil
}

func (p *Plugin) deliver(entries []map[string]any, _ int) (int, error) {
	for i, raw := range entries {
		entry, ok := raw["entry"].(metricEntry)
		if !ok {
			return i + 1, fmt.Errorf("invalid Datadog metric entry %T", raw["entry"])
		}
		if err := p.send(entry); err != nil {
			return i + 1, err
		}
	}
	return 0, nil
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
