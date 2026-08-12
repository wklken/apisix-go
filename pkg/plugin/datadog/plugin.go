package datadog

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	metadata Metadata

	BatchProcessor *logger_batch.Processor
	RouteID        string
	ServerAddr     string

	// dialFunc is the DogStatsD socket factory; retained so tests can count
	// dials. The socket itself is reused across sends and closed on Stop.
	dialFunc func() (net.Conn, error)
	connMu   sync.Mutex
	conn     net.Conn
}

const (
	priority        = 495
	name            = "datadog"
	maxDatagramSize = 8192

	datadogSendTimeout = time.Second
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
        "maxLength": 200,
        "pattern": "^[\\p{L}](?:[\\p{L}\\p{N}_.:/-]*[\\p{L}\\p{N}_./-])?$"
      },
      "default": []
    },
    "name": {
      "type": "string",
      "default": "datadog"
    },
    "host": {
      "type": "string"
    },
    "port": {
      "type": "integer",
      "minimum": 1,
      "maximum": 65535
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

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "host": {
      "type": "string",
      "default": "127.0.0.1"
    },
    "port": {
      "type": "integer",
      "minimum": 0,
      "default": 8125
    },
    "namespace": {
      "type": "string",
      "default": "apisix"
    },
    "constant_tags": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1,
        "maxLength": 200,
        "pattern": "^[\\p{L}](?:[\\p{L}\\p{N}_.:/-]*[\\p{L}\\p{N}_./-])?$"
      },
      "default": ["source:apisix"]
    }
  }
}
`

type Config struct {
	PreferName      bool     `json:"prefer_name,omitempty"`
	IncludePath     bool     `json:"include_path,omitempty"`
	IncludeMethod   bool     `json:"include_method,omitempty"`
	ConstantTags    []string `json:"constant_tags,omitempty"`
	Host            string   `json:"host,omitempty"`
	Port            int      `json:"port,omitempty"`
	BatchName       string   `json:"name,omitempty"`
	BatchMaxSize    int      `json:"batch_max_size,omitempty"`
	MaxRetryCount   int      `json:"max_retry_count,omitempty"`
	RetryDelay      int      `json:"retry_delay,omitempty"`
	BufferDuration  int      `json:"buffer_duration,omitempty"`
	InactiveTimeout int      `json:"inactive_timeout,omitempty"`
	preferNameSet   bool
	retryDelaySet   bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type configJSON struct {
		PreferName      *bool    `json:"prefer_name,omitempty"`
		IncludePath     bool     `json:"include_path,omitempty"`
		IncludeMethod   bool     `json:"include_method,omitempty"`
		ConstantTags    []string `json:"constant_tags,omitempty"`
		Host            string   `json:"host,omitempty"`
		Port            int      `json:"port,omitempty"`
		BatchName       string   `json:"name,omitempty"`
		BatchMaxSize    int      `json:"batch_max_size,omitempty"`
		MaxRetryCount   int      `json:"max_retry_count,omitempty"`
		RetryDelay      *int     `json:"retry_delay,omitempty"`
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
	c.Host = decoded.Host
	c.Port = decoded.Port
	c.BatchName = decoded.BatchName
	c.BatchMaxSize = decoded.BatchMaxSize
	c.MaxRetryCount = decoded.MaxRetryCount
	if decoded.RetryDelay != nil {
		c.RetryDelay = *decoded.RetryDelay
		c.retryDelaySet = true
	}
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
	LatencyMS          int64
	UpstreamLatency    int64
	HasUpstreamLatency bool
	ApisixLatency      int64
	IngressSize        int64
	EgressSize         int64
	Status             int
	RouteID            string
	RouteName          string
	ServiceID          string
	ServiceName        string
	ConsumerName       string
	BalancerIP         string
	Path               string
	Method             string
	Scheme             string
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema
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
	if p.config.RetryDelay == 0 && !p.config.retryDelaySet {
		p.config.RetryDelay = int(logger_batch.DefaultRetryDelay / time.Second)
	}
	if p.config.BufferDuration == 0 {
		p.config.BufferDuration = int(logger_batch.DefaultBufferDuration / time.Second)
	}
	if p.config.InactiveTimeout == 0 {
		p.config.InactiveTimeout = int(logger_batch.DefaultInactiveTimeout / time.Second)
	}
	p.metadata = loadMetadata()
	if p.config.Host != "" {
		p.metadata.Host = p.config.Host
	}
	if p.config.Port != 0 {
		p.metadata.Port = p.config.Port
	}
	p.BatchProcessor = base.NewBatchProcessor(p.config.BatchName, base.BatchDefaults{
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		RetryDelaySet:      p.config.retryDelaySet,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		PluginID:           name,
	}, p.RouteID, p.ServerAddr, p.deliver)
	return nil
}

func (p *Plugin) SetRouteContext(routeID string, serverAddr string) {
	p.RouteID = routeID
	p.ServerAddr = serverAddr
}

func (p *Plugin) LogCapturePolicy() base.LogCapturePolicy {
	return base.LogCapturePolicy{}
}

// RunLogPhase emits the detached metric snapshot without consulting a live
// request or response writer. The metric payload intentionally mirrors the
// legacy Handler fields.
func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	latency := int64(0)
	if !snapshot.Started.IsZero() && !snapshot.Finished.IsZero() {
		latency = snapshot.Finished.Sub(snapshot.Started).Milliseconds()
	}
	upstreamLatency, hasUpstreamLatency := snapshotInt64(snapshot, "$upstream_latency")
	entry := metricEntry{
		LatencyMS:          latency,
		UpstreamLatency:    upstreamLatency,
		HasUpstreamLatency: hasUpstreamLatency,
		ApisixLatency:      apisixLatency(latency, upstreamLatency),
		IngressSize:        max(snapshot.Request.ContentLength, 0),
		EgressSize:         snapshot.Outcome.Bytes,
		Status:             snapshot.Outcome.Status,
		RouteID:            snapshotString(snapshot, "$route_id"),
		RouteName:          snapshotString(snapshot, "$route_name"),
		ServiceID:          snapshotString(snapshot, "$service_id"),
		ServiceName:        snapshotString(snapshot, "$service_name"),
		ConsumerName:       snapshot.Request.Consumer.Username,
		BalancerIP:         snapshotString(snapshot, "$balancer_ip"),
		Path:               snapshotPath(snapshot),
		Method:             snapshot.Request.Method,
		Scheme:             snapshot.Request.Scheme,
	}
	return base.EnqueueLog(p.BatchProcessor, map[string]any{"entry": entry})
}

func snapshotString(snapshot base.LogSnapshot, key string) string {
	return fmt.Sprint(base.SnapshotValue(snapshot, key))
}

func snapshotInt64(snapshot base.LogSnapshot, key string) (int64, bool) {
	value := base.SnapshotValue(snapshot, key)
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	default:
		parsed, err := strconv.ParseInt(fmt.Sprint(value), 10, 64)
		return parsed, err == nil
	}
}

func snapshotPath(snapshot base.LogSnapshot) string {
	if path := snapshotString(snapshot, "$matched_uri"); path != "" {
		return path
	}
	return snapshot.Request.URI
}

func (p *Plugin) Stop() {
	cleanup := func() {
		p.connMu.Lock()
		if p.conn != nil {
			_ = p.conn.Close()
			p.conn = nil
		}
		p.connMu.Unlock()
	}
	if p.BatchProcessor != nil {
		p.BatchProcessor.StopWithCleanup(cleanup)
		return
	}
	cleanup()
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		captured := httpsnoop.CaptureMetrics(next, w, r)
		upstreamLatency, hasUpstreamLatency := requestInt64Var(r, "$upstream_latency")
		entry := metricEntry{
			LatencyMS:          captured.Duration.Milliseconds(),
			UpstreamLatency:    upstreamLatency,
			HasUpstreamLatency: hasUpstreamLatency,
			ApisixLatency:      apisixLatency(captured.Duration.Milliseconds(), upstreamLatency),
			IngressSize:        util.RequestSize(r),
			EgressSize:         captured.Written,
			Status:             captured.Code,
			RouteID:            apisixStringVar(r, "$route_id"),
			RouteName:          apisixStringVar(r, "$route_name"),
			ServiceID:          apisixStringVar(r, "$service_id"),
			ServiceName:        apisixStringVar(r, "$service_name"),
			ConsumerName:       consumerName(r),
			BalancerIP:         apisixStringVar(r, "$balancer_ip"),
			Path:               matchedPath(r),
			Method:             r.Method,
			Scheme:             requestScheme(r),
		}
		p.BatchProcessor.Push(map[string]any{"entry": entry})
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(entry metricEntry) {
	if err := p.send(context.Background(), entry); err != nil {
		logger.Errorf("failed to send DogStatsD metrics: %s", err)
	}
}

func (p *Plugin) send(ctx context.Context, entry metricEntry) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lines := p.metricLines(entry)
	payload := strings.Join(lines, "\n")

	p.connMu.Lock()
	defer p.connMu.Unlock()

	conn := p.conn
	if conn == nil {
		var err error
		conn, err = p.dial(ctx)
		if err != nil {
			return fmt.Errorf("connect to DogStatsD endpoint %s: %w", p.dogstatsdAddr(), err)
		}
		p.conn = conn
	}
	deadline := time.Now().Add(datadogSendTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetWriteDeadline(deadline)
	stopWatcher := watchConnectionCancellation(ctx, conn)
	defer stopWatcher()

	if len(payload) <= maxDatagramSize {
		if _, err := conn.Write([]byte(payload)); err != nil {
			p.resetConnLocked(conn)
			return fmt.Errorf("send DogStatsD metrics: %w", err)
		}
		return nil
	}

	for _, line := range lines {
		if _, err := conn.Write([]byte(line)); err != nil {
			p.resetConnLocked(conn)
			return fmt.Errorf("send DogStatsD metric %q: %w", line, err)
		}
	}
	return nil
}

func (p *Plugin) dogstatsdAddr() string {
	return net.JoinHostPort(p.metadata.Host, strconv.Itoa(p.metadata.Port))
}

func (p *Plugin) dial(ctx context.Context) (net.Conn, error) {
	if p.dialFunc != nil {
		return p.dialFunc()
	}
	dialer := &net.Dialer{Timeout: datadogSendTimeout}
	return dialer.DialContext(ctx, "udp", p.dogstatsdAddr())
}

func (p *Plugin) resetConnLocked(conn net.Conn) {
	_ = conn.Close()
	if p.conn == conn {
		p.conn = nil
	}
}

func (p *Plugin) deliver(ctx context.Context, entries []map[string]any, _ int) (int, error) {
	for i, raw := range entries {
		entry, ok := raw["entry"].(metricEntry)
		if !ok {
			return i + 1, fmt.Errorf("invalid Datadog metric entry %T", raw["entry"])
		}
		if err := p.send(ctx, entry); err != nil {
			return i + 1, err
		}
	}
	return 0, nil
}

func watchConnectionCancellation(ctx context.Context, conn net.Conn) func() {
	if ctx == nil {
		return func() {}
	}
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Go(func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	})
	return func() {
		close(done)
		wait.Wait()
	}
}

func (p *Plugin) metricLines(entry metricEntry) []string {
	tags := p.generateTags(entry)
	lines := []string{
		p.metricLine("request.counter", "1", "c", tags),
		p.metricLine("request.latency", strconv.FormatInt(entry.LatencyMS, 10), "h", tags),
	}
	if entry.HasUpstreamLatency {
		lines = append(
			lines,
			p.metricLine("upstream.latency", strconv.FormatInt(entry.UpstreamLatency, 10), "h", tags),
		)
	}
	lines = append(lines,
		p.metricLine("apisix.latency", strconv.FormatInt(entry.ApisixLatency, 10), "h", tags),
		p.metricLine("ingress.size", strconv.FormatInt(entry.IngressSize, 10), "ms", tags),
		p.metricLine("egress.size", strconv.FormatInt(entry.EgressSize, 10), "ms", tags),
	)
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
	if route := resourceTag(entry.RouteID, entry.RouteName, p.config.PreferName); route != "" {
		tags = append(tags, "route_name:"+route)
	}
	if p.config.IncludePath && entry.Path != "" {
		tags = append(tags, "path:"+entry.Path)
	}
	if p.config.IncludeMethod && entry.Method != "" {
		tags = append(tags, "method:"+entry.Method)
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

func requestInt64Var(r *http.Request, key string) (int64, bool) {
	switch value := ctx.GetRequestVar(r, key).(type) {
	case int64:
		return value, true
	case int:
		return int64(value), true
	case float64:
		return int64(value), true
	default:
		return 0, false
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

func requestScheme(r *http.Request) string {
	if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
		return scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func loadMetadata() Metadata {
	return metadataWithDefaults(base.LoadPluginMetadata[Metadata](name))
}

func metadataWithDefaults(configured Metadata) Metadata {
	metadata := Metadata{
		Host:         "127.0.0.1",
		Port:         8125,
		Namespace:    "apisix",
		ConstantTags: []string{"source:apisix"},
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
