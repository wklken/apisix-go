package loggly

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config      Config
	lifecycleMu sync.RWMutex
	stopped     atomic.Bool

	token           secret.Value
	tokenSet        bool
	secretsPrepared bool
	ready           bool

	// dialFunc is the syslog socket factory; retained so tests can count
	// dials. The socket is reused across messages and closed on Stop.
	dialFunc   func() (net.Conn, error)
	connMu     sync.Mutex
	conn       net.Conn
	httpClient *http.Client
}

const (
	priority          = 411
	name              = "loggly"
	version           = "apisix-go"
	logglyHostField   = "\x00loggly_host"
	logglyStatusField = "\x00loggly_status"
)

const schema = `
{
  "type": "object",
  "properties": {
    "customer_token": {
      "type": "string"
    },
    "severity": {
      "type": "string",
      "default": "INFO",
      "enum": ["DEBUG", "INFO", "NOTICE", "WARNING", "ERR", "CRIT", "ALERT", "EMEGR", "debug", "info", "notice", "warning", "err", "crit", "alert", "emegr"]
    },
    "severity_map": {
      "type": "object"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "include_req_body": {
      "type": "boolean",
      "default": false
    },
    "include_req_body_expr": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "array"
      }
    },
    "include_resp_body": {
      "type": "boolean",
      "default": false
    },
    "include_resp_body_expr": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "array"
      }
    },
    "max_req_body_bytes": {
      "type": "integer",
      "minimum": 1,
      "default": 524288
    },
    "max_resp_body_bytes": {
      "type": "integer",
      "minimum": 1,
      "default": 524288
    },
    "tags": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      },
      "default": ["apisix"]
    },
    "log_format": {
      "type": "object"
    },
    "host": {
      "type": "string",
      "default": "logs-01.loggly.com"
    },
    "port": {
      "type": "integer",
      "default": 514
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5000
    },
    "protocol": {
      "type": "string",
      "default": "syslog",
      "enum": ["syslog", "http", "https"]
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
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1
    }
  },
  "required": ["customer_token"]
}
`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "host": {
      "type": "string"
    },
    "port": {
      "type": "integer"
    },
    "protocol": {
      "type": "string"
    },
    "timeout": {
      "type": "integer"
    },
    "log_format": {
      "type": "object",
      "additionalProperties": {
        "type": "string"
      }
    }
  }
}
`

type pluginMetadata struct {
	Host      string            `json:"host,omitempty"`
	Port      int               `json:"port,omitempty"`
	Protocol  string            `json:"protocol,omitempty"`
	Timeout   int               `json:"timeout,omitempty"`
	LogFormat map[string]string `json:"log_format,omitempty"`
}

type Config struct {
	CustomerToken string            `json:"customer_token"`
	Severity      string            `json:"severity,omitempty"`
	SeverityMap   map[string]string `json:"severity_map,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	SSLVerify     *bool             `json:"ssl_verify,omitempty"`
	LogFormat     map[string]string `json:"log_format,omitempty"`
	Host          string            `json:"host,omitempty"`
	Port          int               `json:"port,omitempty"`
	Timeout       int               `json:"timeout,omitempty"`
	Protocol      string            `json:"protocol,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
}

var severityValues = map[string]int{
	"EMEGR":   0,
	"ALERT":   1,
	"CRIT":    2,
	"ERR":     3,
	"WARNING": 4,
	"NOTICE":  5,
	"INFO":    6,
	"DEBUG":   7,
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	p.InitLogger(p.Send)

	return nil
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopped.Load() {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}
	value, err := access.Materialize(ctx, "customer_token", p.config.CustomerToken)
	if err != nil || value.Use(validateLogglyToken) != nil {
		return logglyTokenUnavailable()
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return logglyTokenUnavailable()
	}
	p.token = value
	p.tokenSet = true
	p.config.CustomerToken = descriptor.String()
	p.secretsPrepared = true
	return nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
// Immutable generation preparation uses MaterializeScopedSecrets instead.
func (p *Plugin) MaterializeSecrets() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopped.Load() {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}
	return logglyTokenUnavailable()
}

func validateLogglyToken(value string) error {
	if strings.TrimSpace(value) == "" {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func logglyTokenUnavailable() error {
	return fmt.Errorf("%s customer_token: %w", name, secret.ErrCredentialUnavailable)
}

func (p *Plugin) PostInit() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopped.Load() || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.ready {
		return nil
	}
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}
	var metadata pluginMetadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("loggly metadata decode failed: %w", err)
	}

	if p.config.Severity == "" {
		p.config.Severity = "INFO"
	}
	p.config.Severity = strings.ToUpper(p.config.Severity)
	if len(p.config.Tags) == 0 {
		p.config.Tags = []string{"apisix"}
	}
	if p.config.Host == "" {
		p.config.Host = metadata.Host
	}
	if p.config.Host == "" {
		p.config.Host = "logs-01.loggly.com"
	}
	if p.config.Port == 0 {
		p.config.Port = metadata.Port
	}
	if p.config.Port == 0 {
		p.config.Port = 514
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = metadata.Timeout
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 5000
	}
	if p.config.Protocol == "" {
		p.config.Protocol = metadata.Protocol
	}
	if p.config.Protocol == "" {
		p.config.Protocol = "syslog"
	}
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
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

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	)

	httpClient := &http.Client{
		Timeout: time.Duration(p.config.Timeout) * time.Millisecond,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !*p.config.SSLVerify},
		},
	}

	processor, err := base.NewBatchProcessor("loggly", p.TaskOwner(), base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.sendBatchFromProcessor)
	if err != nil {
		httpClient.CloseIdleConnections()
		return err
	}
	if p.stopped.Load() {
		processor.Stop()
		httpClient.CloseIdleConnections()
		return secret.ErrCredentialUnavailable
	}
	p.httpClient = httpClient
	p.BatchProcessor = processor
	p.ready = true

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		request := base.CaptureMinimalAccessLogRequest(r, started)
		if len(p.LogFormat) == 0 {
			request = base.CaptureAccessLogRequest(r, started, p.ServerAddr)
		}

		var requestBody string
		if p.config.IncludeReqBody && base.ExprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := base.ReadSharedRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *base.SharedResponseRecorder
		if p.config.IncludeRespBody {
			recorder = base.GetOrCreateSharedResponseRecorderWithLimit(w, r, p.config.MaxRespBodyBytes)
			writer = recorder
		}

		metrics := httpsnoop.CaptureMetrics(next, writer, r)
		var logFields map[string]any
		if len(p.LogFormat) > 0 {
			logFields = resolveLogFormat(r, request, p.LogFormat)
			logFields["route_id"] = p.RouteID
		} else {
			logFields = base.BuildAccessLogSnapshot(
				request,
				metrics.Code,
				w.Header(),
				metrics.Written,
				p.RouteID,
				r,
				metrics.Duration,
			)
		}

		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() &&
			base.ExprMatched(r, p.config.IncludeRespBodyExpr, metrics.Code) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
		}
		logFields[logglyHostField] = request.Host
		logFields[logglyStatusField] = metrics.Code

		_ = p.enqueueLogglyIfRunning(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	var fields map[string]any
	if len(p.LogFormat) > 0 {
		fields = resolveLogglySnapshotFormat(snapshot, p.LogFormat)
		if routeID := fmt.Sprint(base.SnapshotValue(snapshot, "$route_id")); routeID != "" {
			fields["route_id"] = routeID
		}
	} else {
		fields = base.BuildAccessLogFromSnapshot(snapshot, "")
	}
	if p.config.IncludeReqBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr) {
		if body := base.SnapshotRequestBody(snapshot, p.config.MaxReqBodyBytes); body != "" {
			base.NestedLogMap(fields, "request")["body"] = body
		}
	}
	if p.config.IncludeRespBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeRespBodyExpr) {
		if body := base.SnapshotResponseBody(snapshot, p.config.MaxRespBodyBytes); body != "" {
			base.NestedLogMap(fields, "response")["body"] = body
		}
	}
	fields[logglyHostField] = base.HostWithoutPort(snapshot.Request.Host)
	fields[logglyStatusField] = snapshot.Outcome.Status
	return p.enqueueLogglyIfRunning(fields)
}

func (p *Plugin) enqueueLogglyIfRunning(fields map[string]any) error {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() || !p.ready {
		return base.ErrLogQueueUnavailable
	}
	if p.BatchProcessor == nil {
		return p.Fire(fields)
	}
	return p.EnqueueLog(fields)
}

func resolveLogglySnapshotFormat(snapshot base.LogSnapshot, format map[string]string) map[string]any {
	return base.ResolveStringLogFormat(format, func(value string) any {
		if value == "$time_iso8601" {
			return snapshot.Started.Format(time.RFC3339)
		}
		return base.SnapshotValue(snapshot, value)
	})
}

type accessRequest = base.AccessLogRequest

func resolveLogFormat(r *http.Request, request accessRequest, format map[string]string) map[string]any {
	return base.ResolveStringLogFormat(format, func(value string) any {
		switch value {
		case "$host":
			return request.Host
		case "$remote_addr":
			return request.ClientIP
		case "$time_iso8601":
			return request.Started.Format(time.RFC3339)
		default:
			return apisixlog.GetField(r, value)
		}
	})
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
	return p.sendBatch(ctx, entries, batchMaxSize, false)
}

func (p *Plugin) sendBatchFromProcessor(
	ctx context.Context,
	entries []map[string]any,
	batchMaxSize int,
) (int, error) {
	return p.sendBatch(ctx, entries, batchMaxSize, true)
}

func (p *Plugin) sendBatch(
	ctx context.Context,
	entries []map[string]any,
	batchMaxSize int,
	allowRetiredDrain bool,
) (int, error) {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if (!allowRetiredDrain && p.stopped.Load()) || !p.secretsPrepared || !p.ready {
		return 0, secret.ErrCredentialUnavailable
	}
	if p.config.Protocol == "http" || p.config.Protocol == "https" {
		payload, err := p.encodeHTTPBulk(entries, batchMaxSize)
		if err != nil {
			return 0, err
		}
		defer clear(payload)
		return 0, p.useTokenLocked(allowRetiredDrain, func(token string) error {
			return p.sendHTTPBulk(ctx, payload, token)
		})
	}

	firstFailed := 0
	err := p.useTokenLocked(allowRetiredDrain, func(token string) error {
		for i, entry := range entries {
			message := p.buildMessage(entry, token)
			if err := p.sendUDPMessage(ctx, message); err != nil {
				firstFailed = i + 1
				return err
			}
		}
		return nil
	})
	return firstFailed, err
}

func (p *Plugin) sendUDPMessage(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.connMu.Lock()
	defer p.connMu.Unlock()

	conn := p.conn
	if conn == nil {
		var err error
		conn, err = p.dial(ctx)
		if err != nil {
			return fmt.Errorf("failed to connect to Loggly UDP endpoint %s:%d: %w", p.config.Host, p.config.Port, err)
		}
		p.conn = conn
	}
	deadline := time.Now().Add(time.Duration(p.config.Timeout) * time.Millisecond)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetWriteDeadline(deadline)
	stopWatcher := watchConnectionCancellation(ctx, conn)
	payload := []byte(message)
	_, writeErr := conn.Write(payload)
	clear(payload)
	stopWatcher()
	if ctxErr := ctx.Err(); ctxErr != nil {
		p.resetConnLocked(conn)
		return ctxErr
	}
	if writeErr != nil {
		p.resetConnLocked(conn)
		return fmt.Errorf("failed to send loggly message: %w", writeErr)
	}
	return nil
}

func (p *Plugin) dial(ctx context.Context) (net.Conn, error) {
	if p.dialFunc != nil {
		return p.dialFunc()
	}
	return (&net.Dialer{Timeout: time.Duration(p.config.Timeout) * time.Millisecond}).DialContext(
		ctx,
		"udp",
		net.JoinHostPort(p.config.Host, strconv.Itoa(p.config.Port)),
	)
}

func (p *Plugin) resetConnLocked(conn net.Conn) {
	_ = conn.Close()
	if p.conn == conn {
		p.conn = nil
	}
}

// Stop drains the batch processor, then closes the retained syslog socket
// and any idle HTTP connections.
func (p *Plugin) QuiesceGenerationTasks() { p.Stop() }

func (p *Plugin) Stop() {
	if p.stopped.Swap(true) {
		return
	}
	p.lifecycleMu.RLock()
	processor := p.BatchProcessor
	p.lifecycleMu.RUnlock()
	cleanup := func() {
		p.lifecycleMu.Lock()
		defer p.lifecycleMu.Unlock()
		p.connMu.Lock()
		if p.conn != nil {
			_ = p.conn.Close()
			p.conn = nil
		}
		p.connMu.Unlock()

		if p.httpClient != nil {
			p.httpClient.CloseIdleConnections()
		}
		p.httpClient = nil
		p.BatchProcessor = nil
		p.token = secret.Value{}
		p.tokenSet = false
		p.secretsPrepared = false
		p.ready = false
	}
	if processor != nil {
		processor.StopWithCleanup(cleanup)
	} else {
		cleanup()
	}
}

func watchConnectionCancellation(ctx context.Context, conn net.Conn) func() {
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

func (p *Plugin) sendHTTPBulk(ctx context.Context, payload []byte, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.bulkEndpoint(token), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build Loggly bulk request: %w", err)
	}
	defer func() {
		req.Body = http.NoBody
		req.GetBody = nil
		req.URL = nil
	}()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LOGGLY-TAG", strings.Join(p.config.Tags, ","))

	resp, err := p.httpClient.Do(req)
	if resp != nil {
		defer func() {
			_ = resp.Body.Close()
			resp.Body = http.NoBody
			resp.Request = nil
		}()
	}
	if err != nil {
		return fmt.Errorf("failed to send loggly bulk message: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to send loggly bulk message: status %d", resp.StatusCode)
	}
	return nil
}

func (p *Plugin) encodeHTTPBulk(entries []map[string]any, batchMaxSize int) ([]byte, error) {
	if batchMaxSize == 1 && len(entries) == 1 {
		payload, err := json.Marshal(logglyPayload(entries[0]))
		if err != nil {
			return nil, fmt.Errorf("failed to marshal loggly message: %w", err)
		}
		return payload, nil
	}

	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		payload, err := json.Marshal(logglyPayload(entry))
		if err != nil {
			return nil, fmt.Errorf("failed to marshal loggly message: %w", err)
		}
		lines = append(lines, string(payload))
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func (p *Plugin) bulkEndpoint(token string) string {
	host := strings.TrimRight(p.config.Host, "/")
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = p.config.Protocol + "://" + host
	}
	return host + "/bulk/" + token + "/tag/bulk"
}

func (p *Plugin) buildMessage(log map[string]any, token string) string {
	payload, err := json.Marshal(logglyPayload(log))
	if err != nil {
		payload = []byte(`{}`)
	}

	hostname, _ := log[logglyHostField].(string)
	if hostname == "" {
		hostname = base.Hostname()
		if hostname == "" {
			hostname = "-"
		}
	}

	return strings.Join([]string{
		fmt.Sprintf("<%d>1", 8+messageSeverity(p.config.Severity, p.config.SeverityMap, log)),
		time.Now().UTC().Format(time.RFC3339Nano),
		hostname,
		"apisix",
		fmt.Sprint(os.Getpid()),
		"-",
		p.structuredData(token),
		string(payload),
	}, " ")
}

func messageSeverity(defaultSeverity string, severityMap map[string]string, log map[string]any) int {
	if status, ok := log[logglyStatusField]; ok {
		key := fmt.Sprint(status)
		if severity, ok := severityMap[key]; ok {
			return severityCode(severity)
		}
	}
	if status, ok := log["status"]; ok {
		key := fmt.Sprint(status)
		if severity, ok := severityMap[key]; ok {
			return severityCode(severity)
		}
	}
	return severityCode(defaultSeverity)
}

func logglyPayload(entry map[string]any) map[string]any {
	_, hasHost := entry[logglyHostField]
	_, hasStatus := entry[logglyStatusField]
	if !hasHost && !hasStatus {
		return entry
	}
	payload := make(map[string]any, len(entry))
	for key, value := range entry {
		if key != logglyHostField && key != logglyStatusField {
			payload[key] = value
		}
	}
	return payload
}

func severityCode(severity string) int {
	if code, ok := severityValues[strings.ToUpper(severity)]; ok {
		return code
	}
	return severityValues["INFO"]
}

func (p *Plugin) structuredData(token string) string {
	tags := make([]string, 0, len(p.config.Tags))
	for _, tag := range p.config.Tags {
		tags = append(tags, fmt.Sprintf(`tag="%s"`, tag))
	}
	if len(tags) == 0 {
		return fmt.Sprintf("[%s@41058]", token)
	}
	return fmt.Sprintf("[%s@41058 %s]", token, strings.Join(tags, " "))
}

func (p *Plugin) useTokenLocked(allowRetiredDrain bool, use func(string) error) error {
	if use == nil || (!allowRetiredDrain && p.stopped.Load()) || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.tokenSet {
		return p.token.Use(use)
	}
	return secret.ErrCredentialUnavailable
}
