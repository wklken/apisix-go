package tencent_cloud_cls

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
	"google.golang.org/protobuf/encoding/protowire"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
	now    func() time.Time
	sample func() float64

	clientRelease func()
	sourceMu      sync.Mutex
	sourceIP      string
	lookupHostIP  func(string) ([]net.IP, error)

	lifecycleMu sync.RWMutex
	stopped     atomic.Bool
	ready       bool
	stopMu      sync.Mutex
	stopDone    chan struct{}

	secretKey       secret.Value
	secretKeySet    bool
	secretsPrepared bool

	// testLifecycleHook is a package-local synchronization seam for lifecycle
	// tests; it is nil in production.
	testLifecycleHook func(string)
}

const (
	priority = 397
	name     = "tencent-cloud-cls"

	defaultScheme      = "https"
	clsAPIPath         = "/structuredlog"
	authExpireSeconds  = 60
	defaultHTTPTimeout = 10 * time.Second

	lifecycleSigningCallbackReturned = "signing-callback-returned"

	maxSingleValueSize   = 1 * 1024 * 1024
	maxLogGroupValueSize = 5 * 1024 * 1024
)

const schema = `
{
  "type": "object",
  "properties": {
    "cls_host": {
      "type": "string"
    },
    "cls_topic": {
      "type": "string"
    },
    "scheme": {
      "type": "string",
      "enum": ["http", "https"],
      "default": "https"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "secret_id": {
      "type": "string"
    },
    "secret_key": {
      "type": "string"
    },
    "sample_ratio": {
      "type": "number",
      "minimum": 0.00001,
      "maximum": 1,
      "default": 1
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
    "global_tag": {
      "type": "object"
    },
    "log_format": {
      "type": "object"
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
  "required": ["cls_host", "cls_topic", "secret_id", "secret_key"]
}
`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "log_format": {
      "type": "object",
      "additionalProperties": {
        "type": "string"
      }
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1
    }
  }
}
`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Config struct {
	CLSHost             string            `json:"cls_host"`
	CLSTopic            string            `json:"cls_topic"`
	Scheme              string            `json:"scheme,omitempty"`
	SSLVerify           *bool             `json:"ssl_verify,omitempty"`
	SecretID            string            `json:"secret_id"`
	SecretKey           string            `json:"secret_key"`
	SampleRatio         float64           `json:"sample_ratio,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any           `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any           `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`
	GlobalTag           map[string]string `json:"global_tag,omitempty"`
	LogFormat           map[string]string `json:"log_format,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
	Timeout           int `json:"-"`
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
	if isSecretReference(p.config.SecretID) {
		return clsSecretKeyUnavailable()
	}

	value, err := access.Materialize(ctx, "secret_key", p.config.SecretKey)
	if err != nil || value.Use(validateCLSSecretKey) != nil {
		return clsSecretKeyUnavailable()
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return clsSecretKeyUnavailable()
	}
	p.secretKey = value
	p.secretKeySet = true
	p.config.SecretKey = descriptor.String()
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
	if isSecretReference(p.config.SecretID) {
		return clsSecretKeyUnavailable()
	}
	return clsSecretKeyUnavailable()
}

func validateCLSSecretKey(value string) error {
	if strings.TrimSpace(value) == "" {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func isSecretReference(value string) bool {
	return strings.HasPrefix(value, "$secret://") ||
		(len(value) >= len("$ENV://") && strings.EqualFold(value[:len("$ENV://")], "$ENV://"))
}

func clsSecretKeyUnavailable() error {
	return fmt.Errorf("%s secret_key: %w", name, secret.ErrCredentialUnavailable)
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
		return fmt.Errorf("tencent-cloud-cls metadata decode failed: %w", err)
	}

	p.applyDefaults()

	logFormat, err := base.RequireStringLogFormat(name, p.config.LogFormat, metadata.LogFormat)
	if err != nil {
		return err
	}
	p.LogFormat = logFormat

	configUID := shared.NewConfigUID()
	configUID.Add(p.sslVerify())
	configUID.Add(p.config.Timeout)

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Millisecond)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.sslVerify()})
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) { return client, nil },
		shared.CloseRestyClient,
	)
	if err != nil {
		return err
	}
	sharedClient := value.(*resty.Client)

	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	)

	processor, err := base.NewBatchProcessor("tencent-cloud-cls", p.TaskOwner(), base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.sendPendingBatch)
	if err != nil {
		release()
		return err
	}
	if p.stopped.Load() {
		processor.Stop()
		release()
		return secret.ErrCredentialUnavailable
	}
	p.client = sharedClient
	p.clientRelease = release
	p.BatchProcessor = processor
	p.ready = true

	return nil
}

func (p *Plugin) QuiesceGenerationTasks() { p.Stop() }

func (p *Plugin) Stop() {
	p.stopMu.Lock()
	if p.stopDone != nil {
		done := p.stopDone
		p.stopMu.Unlock()
		<-done
		return
	}
	done := make(chan struct{})
	p.stopDone = done
	p.stopped.Store(true)
	p.stopMu.Unlock()
	defer close(done)
	p.lifecycleMu.RLock()
	processor := p.BatchProcessor
	p.lifecycleMu.RUnlock()
	cleanup := func() {
		p.lifecycleMu.Lock()
		defer p.lifecycleMu.Unlock()
		if p.clientRelease != nil {
			p.clientRelease()
			p.clientRelease = nil
		}
		p.client = nil
		p.BatchProcessor = nil
		p.ready = false
		p.secretKey = secret.Value{}
		p.secretKeySet = false
		p.secretsPrepared = false
	}
	if processor != nil {
		processor.StopWithCleanup(cleanup)
	} else {
		cleanup()
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	if p.config.IncludeReqBody || p.config.IncludeRespBody {
		return p.bodyAwareHandler(next)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if p.config.SampleRatio < 1 && p.sampleValue() >= p.config.SampleRatio {
			return
		}
		fields := apisixlog.GetFields(r, p.LogFormat)
		base.ApplyRequestMatchedRouteFields(fields, r, p.RouteID)
		_ = p.enqueueLogIfRunning(fields)
	})
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	if p.config.SampleRatio < 1 && p.sampleValue() >= p.config.SampleRatio {
		return nil
	}
	fields := base.GetFieldsFromSnapshot(snapshot, p.LogFormat)
	base.ApplySnapshotMatchedRouteFields(fields, snapshot, p.RouteID)
	if p.config.IncludeReqBody &&
		base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr) {
		if body := base.SnapshotRequestBody(snapshot, p.config.MaxReqBodyBytes); body != "" {
			base.NestedLogMap(fields, "request")["body"] = body
		}
	}
	if p.config.IncludeRespBody &&
		base.SnapshotExpressionMatches(snapshot, p.config.IncludeRespBodyExpr) {
		if body := base.SnapshotResponseBody(snapshot, p.config.MaxRespBodyBytes); body != "" {
			base.NestedLogMap(fields, "response")["body"] = body
		}
	}
	return p.enqueueLogIfRunning(fields)
}

func (p *Plugin) enqueueLogIfRunning(fields map[string]any) error {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() || !p.ready || p.BatchProcessor == nil {
		return base.ErrLogQueueUnavailable
	}
	return p.EnqueueLog(fields)
}

func (p *Plugin) sampleValue() float64 {
	if p.sample != nil {
		return p.sample()
	}
	return rand.Float64()
}

func (p *Plugin) bodyAwareHandler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		sampled := p.config.SampleRatio >= 1 || p.sampleValue() < p.config.SampleRatio

		var requestBody string
		if sampled && p.config.IncludeReqBody && base.ExprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := base.ReadSharedRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *base.SharedResponseRecorder
		if sampled && p.config.IncludeRespBody {
			recorder = base.GetOrCreateSharedResponseRecorderWithLimit(w, r, p.config.MaxRespBodyBytes)
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		if !sampled {
			return
		}
		status := 0
		if recorder != nil {
			status = recorder.StatusCode()
		}

		logFields := apisixlog.GetFields(r, p.LogFormat)
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
		}
		_ = p.enqueueLogIfRunning(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(
	ctx context.Context,
	entries []map[string]any,
	batchMaxSize int,
) (int, error) {
	return p.sendBatch(ctx, entries, batchMaxSize, false)
}

func (p *Plugin) sendPendingBatch(
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
	allowRetired bool,
) (int, error) {
	_ = batchMaxSize
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if (!allowRetired && p.stopped.Load()) || !p.ready || p.client == nil {
		return 0, secret.ErrCredentialUnavailable
	}
	payload, err := p.buildBatchPayload(entries)
	if err != nil {
		return 0, fmt.Errorf("resolve Tencent Cloud CLS source: %w", err)
	}
	defer clear(payload)
	if len(payload) == 0 {
		return 0, nil
	}

	endpoint := p.endpointURL()
	err = p.useSigningConfigLocked(func(config *Config) error {
		request := p.client.R().
			SetContext(ctx).
			SetHeader("Host", p.config.CLSHost).
			SetHeader("Content-Type", "application/x-protobuf").
			SetBody(payload)
		var resp *resty.Response
		defer func() {
			request.Header.Del("Authorization")
			request.Body = nil
			if request.RawRequest != nil {
				request.RawRequest.Header.Del("Authorization")
				request.RawRequest.Body = http.NoBody
				request.RawRequest.GetBody = nil
			}
			if resp != nil {
				if body := resp.Body(); len(body) > 0 {
					clear(body)
					resp.SetBody(nil)
				}
				if resp.RawResponse != nil {
					resp.RawResponse.Header.Del("Authorization")
					resp.RawResponse.Request = nil
					resp.RawResponse.Body = http.NoBody
				}
				resp.Request = nil
				resp.RawResponse = nil
			}
		}()

		request.SetHeader("Authorization", authorization(config, p.now()))
		var err error
		resp, err = request.Post(endpoint)
		if err != nil {
			return fmt.Errorf(
				"failed to send log to Tencent Cloud CLS endpoint %s: %w",
				endpoint, err,
			)
		}
		if resp.StatusCode() >= 300 {
			return fmt.Errorf(
				"tencent Cloud CLS endpoint returned status code [%d] uri [%s], body [%s]",
				resp.StatusCode(),
				endpoint,
				resp.String(),
			)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, secret.ErrCredentialUnavailable) {
			return 0, fmt.Errorf(
				"failed to send log to Tencent Cloud CLS endpoint %s: %w",
				endpoint, err,
			)
		}
		return 0, err
	}
	return 0, nil
}

func (p *Plugin) useSigningConfigLocked(use func(*Config) error) error {
	if use == nil || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	privateConfig := Config{SecretID: p.config.SecretID}
	defer func() { privateConfig = Config{} }()
	usePrivateConfig := func() error {
		err := use(&privateConfig)
		if hook := p.testLifecycleHook; hook != nil {
			hook(lifecycleSigningCallbackReturned)
		}
		return err
	}
	if p.secretKeySet {
		return p.secretKey.Use(func(value string) error {
			privateConfig.SecretKey = value
			defer func() { privateConfig.SecretKey = "" }()
			return usePrivateConfig()
		})
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) applyDefaults() {
	if p.config.Scheme == "" {
		p.config.Scheme = defaultScheme
	}
	if p.config.SSLVerify == nil {
		verify := true
		p.config.SSLVerify = &verify
	}
	if p.config.SampleRatio == 0 {
		p.config.SampleRatio = 1
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = int(defaultHTTPTimeout / time.Millisecond)
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
	if p.now == nil {
		p.now = time.Now
	}
}

func (p *Plugin) sslVerify() bool {
	return p.config.SSLVerify == nil || *p.config.SSLVerify
}

func (p *Plugin) endpointURL() string {
	values := url.Values{}
	values.Set("topic_id", p.config.CLSTopic)
	return fmt.Sprintf("%s://%s%s?%s", p.config.Scheme, p.config.CLSHost, clsAPIPath, values.Encode())
}

func authorization(config *Config, now time.Time) string {
	signTime := fmt.Sprintf("%d;%d", now.Unix(), now.Unix()+authExpireSeconds)
	httpRequestInfo := fmt.Sprintf("%s\n%s\n%s\n%s\n", "post", clsAPIPath, "", "")
	stringToSign := fmt.Sprintf("%s\n%s\n%s\n", "sha1", signTime, sha1Hex([]byte(httpRequestInfo)))
	secretKey := []byte(config.SecretKey)
	defer clear(secretKey)
	signKeyDigest := hmacSHA1(secretKey, []byte(signTime))
	defer clear(signKeyDigest)
	signKey := make([]byte, hex.EncodedLen(len(signKeyDigest)))
	hex.Encode(signKey, signKeyDigest)
	defer clear(signKey)
	signatureDigest := hmacSHA1(signKey, []byte(stringToSign))
	defer clear(signatureDigest)
	signature := hex.EncodeToString(signatureDigest)

	return "q-sign-algorithm=sha1" +
		"&q-ak=" + config.SecretID +
		"&q-sign-time=" + signTime +
		"&q-key-time=" + signTime +
		"&q-header-list=" +
		"&q-url-param-list=" +
		"&q-signature=" + signature
}

func (p *Plugin) buildBatchPayload(logs []map[string]any) ([]byte, error) {
	sourceIP, err := p.resolveSourceIP()
	if err != nil {
		return nil, err
	}
	group := []byte(nil)
	totalSize := 0
	truncatedValues := 0
	droppedEntries := 0
	for i, logEntry := range logs {
		contents, size, truncated := normalizeLog(logEntry, p.config.GlobalTag)
		truncatedValues += truncated
		if size > maxLogGroupValueSize {
			droppedEntries++
			continue
		}
		totalSize += size
		if totalSize > maxLogGroupValueSize {
			droppedEntries += len(logs) - i
			break
		}
		group = appendBytesField(group, 1, appendLog(nil, p.now().UnixMilli(), contents))
	}
	if truncatedValues > 0 {
		logger.Warnf("Tencent Cloud CLS truncated %d field value(s) over the 1MB single-value limit", truncatedValues)
	}
	if droppedEntries > 0 {
		logger.Errorf("Tencent Cloud CLS dropped %d log(s) over the 5MB limit", droppedEntries)
	}
	if len(group) == 0 {
		return nil, nil
	}
	group = appendStringField(group, 4, sourceIP)
	return appendBytesField(nil, 1, group), nil
}

func (p *Plugin) resolveSourceIP() (string, error) {
	p.sourceMu.Lock()
	defer p.sourceMu.Unlock()
	if p.sourceIP != "" {
		return p.sourceIP, nil
	}
	hostname := base.Hostname()
	if hostname == "" {
		return "", fmt.Errorf("local hostname is empty")
	}
	lookup := p.lookupHostIP
	if lookup == nil {
		lookup = net.LookupIP
	}
	addresses, err := lookup(hostname)
	if err != nil {
		return "", fmt.Errorf("resolve hostname %q: %w", hostname, err)
	}
	for _, address := range addresses {
		if address != nil {
			p.sourceIP = address.String()
			return p.sourceIP, nil
		}
	}
	return "", fmt.Errorf("resolve hostname %q: no IP addresses", hostname)
}

type clsContent struct {
	key   string
	value string
}

func normalizeLog(log map[string]any, globalTag map[string]string) ([]clsContent, int, int) {
	contents := make([]clsContent, 0, len(log)+len(globalTag))
	size := 4
	truncated := 0
	for key, value := range log {
		normalized := normalizeValue(value)
		if len(normalized) > maxSingleValueSize {
			normalized = normalized[:maxSingleValueSize]
			truncated++
		}
		contents = append(contents, clsContent{key: key, value: normalized})
		size += len(key) + len(normalized)
	}
	for key, value := range globalTag {
		contents = append(contents, clsContent{key: key, value: value})
		size += len(key) + len(value)
	}
	return contents, size, truncated
}

func normalizeValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case fmt.Stringer:
		return v.String()
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case bool:
		return strconv.FormatBool(v)
	default:
		if payload, err := json.Marshal(v); err == nil {
			return string(payload)
		}
		return fmt.Sprint(v)
	}
}

func appendLog(buf []byte, timestamp int64, contents []clsContent) []byte {
	buf = protowire.AppendTag(buf, 1, protowire.VarintType)
	buf = protowire.AppendVarint(buf, uint64(timestamp))
	for _, content := range contents {
		raw := appendStringField(nil, 1, content.key)
		raw = appendStringField(raw, 2, content.value)
		buf = appendBytesField(buf, 2, raw)
	}
	return buf
}

func appendStringField(buf []byte, number protowire.Number, value string) []byte {
	return appendBytesField(buf, number, []byte(value))
}

func appendBytesField(buf []byte, number protowire.Number, value []byte) []byte {
	buf = protowire.AppendTag(buf, number, protowire.BytesType)
	buf = protowire.AppendBytes(buf, value)
	return buf
}

func sha1Hex(value []byte) string {
	sum := sha1.Sum(value)
	return hex.EncodeToString(sum[:])
}

func hmacSHA1(key []byte, value []byte) []byte {
	mac := hmac.New(sha1.New, key)
	mac.Write(value)
	return mac.Sum(nil)
}
