package elasticsearch_logger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
)

const (
	// version  = "0.1"
	priority = 413
	name     = "elasticsearch-logger"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "endpoint_addr": {
		"type": "string",
		"pattern": "[^/]$"
	  },
	  "endpoint_addrs": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "string",
		  "pattern": "[^/]$"
		}
	  },
	  "field": {
		"type": "object",
		"properties": {
		  "index": {
			"type": "string"
		  },
		  "type": {
			"type": "string"
		  }
		},
		"required": ["index"]
	  },
	  "log_format": {
		"type": "object"
	  },
	  "auth": {
		"type": "object",
		"properties": {
		  "username": {
			"type": "string",
			"minLength": 1
		  },
		  "password": {
			"type": "string",
			"minLength": 1
		  }
		},
		"required": ["username", "password"]
	  },
	  "headers": {
		"type": "object",
		"minProperties": 1,
		"patternProperties": {
		  "^[^:]+$": {
			"type": "string",
			"minLength": 1
		  }
		},
		"additionalProperties": false
	  },
	  "timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 10
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
	  "batch_max_size": {
		"type": "integer",
		"minimum": 1,
		"default": 1000
	  },
	  "inactive_timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 5
	  },
	  "buffer_duration": {
		"type": "integer",
		"minimum": 1,
		"default": 60
	  },
	  "retry_delay": {
		"type": "integer",
		"minimum": 0,
		"default": 1
	  },
	  "max_retry_count": {
		"type": "integer",
		"minimum": 0,
		"default": 0
	  },
	  "max_pending_entries": {
		"type": "integer",
		"minimum": 1
	  }
	},
	"oneOf": [
	  {"required": ["endpoint_addr", "field"]},
	  {"required": ["endpoint_addrs", "field"]}
	]
}`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "log_format": {
      "type": "object",
      "additionalProperties": {"type": "string"}
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1
    }
  }
}`

const elasticsearchIndexField = "__elasticsearch_logger_index"

// NOTE: not support
// "encrypt_fields": ["auth.password"],
// endpoint_addr is deprecated, use endpoint_addrs instead

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	versionMu sync.Mutex
	esVersion string

	clientMu sync.Mutex
	clients  map[esClientKey]*esClientRef

	secretMu              sync.RWMutex
	password              *secret.Value
	authorization         *secret.Value
	stopped               atomic.Bool
	stopBeforeLock        func()
	postInitBeforePublish func(*logger_batch.Processor)
	cleanupOnce           sync.Once
}

type esClientRef struct {
	client      *elasticsearch.BaseClient
	credentials *elasticsearchCredentialTransport
	release     func()
}

type esClientKey struct {
	endpoint            string
	passwordDigest      [sha256.Size]byte
	authorizationDigest [sha256.Size]byte
}

type elasticsearchCredentialTransport struct {
	owner       *Plugin
	transport   http.RoundTripper
	override    bool
	afterDerive func([]byte)
}

var randomEndpointIndex = rand.Intn

type Config struct {
	EndpointAddr  string            `json:"endpoint_addr,omitempty"`
	EndpointAddrs []string          `json:"endpoint_addrs"`
	Field         FieldConfig       `json:"field"`
	LogFormat     map[string]string `json:"log_format,omitempty"`
	Auth          *AuthConfig       `json:"auth,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Timeout       int               `json:"timeout,omitempty"`
	SslVerify     *bool             `json:"ssl_verify,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
}

type FieldConfig struct {
	Index string  `json:"index"`
	Type  *string `json:"type,omitempty"`
}

type AuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
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

// MaterializeScopedSecrets admits only the two exact catalog-declared fields.
// Public config retains content descriptors while plaintext remains private.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped.Load() {
		return secret.ErrCredentialUnavailable
	}
	if p.credentialsInstalled() {
		return nil
	}
	passwordRaw, hasPassword, authorizationRaw, hasAuthorization := p.rawCredentials()
	if !hasPassword && !hasAuthorization {
		return nil
	}

	var password, authorization secret.Value
	var passwordDescriptor, authorizationDescriptor string
	var err error
	if hasPassword {
		password, err = access.Materialize(ctx, "auth.password", passwordRaw)
		if err != nil || validateElasticsearchSecret(password) != nil {
			return fmt.Errorf("%s auth.password: %w", name, secret.ErrCredentialUnavailable)
		}
		passwordDescriptor, err = scopedElasticsearchDescriptor(password)
		if err != nil {
			return fmt.Errorf("%s auth.password: %w", name, secret.ErrCredentialUnavailable)
		}
	}
	if hasAuthorization {
		authorization, err = access.Materialize(ctx, "headers.Authorization", authorizationRaw)
		if err != nil || validateElasticsearchSecret(authorization) != nil {
			password = secret.Value{}
			return fmt.Errorf("%s headers.Authorization: %w", name, secret.ErrCredentialUnavailable)
		}
		authorizationDescriptor, err = scopedElasticsearchDescriptor(authorization)
		if err != nil {
			password = secret.Value{}
			authorization = secret.Value{}
			return fmt.Errorf("%s headers.Authorization: %w", name, secret.ErrCredentialUnavailable)
		}
	}

	if hasPassword {
		p.config.Auth.Password = passwordDescriptor
		p.password = &password
	}
	if hasAuthorization {
		p.config.Headers["Authorization"] = authorizationDescriptor
		p.authorization = &authorization
	}
	return nil
}

func (p *Plugin) rawCredentials() (
	password string, hasPassword bool, authorization string, hasAuthorization bool,
) {
	if p.config.Auth != nil {
		password, hasPassword = p.config.Auth.Password, true
	}
	if p.config.Headers != nil {
		authorization, hasAuthorization = p.config.Headers["Authorization"]
	}
	return
}

func (p *Plugin) credentialsInstalled() bool {
	passwordReady := p.config.Auth == nil || p.password != nil
	_, authorizationConfigured := p.config.Headers["Authorization"]
	authorizationReady := !authorizationConfigured || p.authorization != nil
	return passwordReady && authorizationReady
}

func validateElasticsearchSecret(value secret.Value) error {
	return value.Use(func(plaintext string) error {
		if strings.TrimSpace(plaintext) == "" {
			return secret.ErrCredentialUnavailable
		}
		return nil
	})
}

func scopedElasticsearchDescriptor(value secret.Value) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (p *Plugin) PostInit() error {
	p.secretMu.RLock()
	prepared := !p.stopped.Load() && p.credentialsInstalled()
	p.secretMu.RUnlock()
	if !prepared {
		return secret.ErrCredentialUnavailable
	}
	var metadata pluginMetadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("%s metadata decode failed: %w", name, err)
	}
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 10
	}
	if p.config.SslVerify == nil {
		sslVerify := true
		p.config.SslVerify = &sslVerify
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
	if len(p.config.EndpointAddrs) == 0 && p.config.EndpointAddr != "" {
		p.config.EndpointAddrs = []string{p.config.EndpointAddr}
	}
	for _, endpoint := range p.config.EndpointAddrs {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(endpoint)), "http://") {
			logger.Warn("Using elasticsearch-logger endpoint_addrs with no TLS is a security risk")
			break
		}
	}

	logFormat, err := base.RequireStringLogFormat(name, p.config.LogFormat, metadata.LogFormat)
	if err != nil {
		return err
	}
	p.LogFormat = logFormat
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	)

	processor, err := base.NewBatchProcessor(name, p.TaskOwner(), base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.SendBatch)
	if err != nil {
		return err
	}
	if p.postInitBeforePublish != nil {
		p.postInitBeforePublish(processor)
	}
	p.secretMu.Lock()
	if p.stopped.Load() {
		p.secretMu.Unlock()
		processor.Stop()
		return secret.ErrCredentialUnavailable
	}
	p.BatchProcessor = processor
	p.secretMu.Unlock()

	// Version detection runs once per stable config at initialization, reusing
	// the pooled client instead of building a transport per attempt.
	if endpoint := p.endpointAddr(); endpoint != "" {
		err := p.withClient(endpoint, func(client *elasticsearch.BaseClient) error {
			p.fetchAndUpdateVersion(client)
			return nil
		})
		if err != nil {
			logger.Errorf("failed to create Elasticsearch client: %s", err)
		}
	}

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
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

		next.ServeHTTP(writer, r)
		status := 0
		if recorder != nil {
			status = recorder.StatusCode()
		}

		logFields := elasticsearchLogFields(r, p.LogFormat)
		base.ApplyRequestMatchedRouteFields(logFields, r, p.RouteID)
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
		}
		logFields[elasticsearchIndexField] = resolveIndexVars(p.config.Field.Index, r)
		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	fields := elasticsearchSnapshotLogFields(snapshot, p.LogFormat)
	base.ApplySnapshotMatchedRouteFields(fields, snapshot, p.RouteID)
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
	fields[elasticsearchIndexField] = resolveIndexVarsSnapshot(p.config.Field.Index, snapshot)
	return p.EnqueueLog(fields)
}

func elasticsearchSnapshotLogFields(snapshot base.LogSnapshot, logFormat map[string]string) map[string]any {
	fields := base.GetFieldsFromSnapshot(snapshot, logFormat)
	for key, value := range logFormat {
		switch value {
		case "$host":
			fields[key] = snapshot.Request.Host
		case "$remote_addr":
			host, _, err := net.SplitHostPort(snapshot.Request.RemoteAddr)
			if err == nil {
				fields[key] = host
			}
		}
	}
	return fields
}

func resolveIndexVarsSnapshot(index string, snapshot base.LogSnapshot) string {
	index = replaceIndexTimeVars(index)
	var out strings.Builder
	for i := 0; i < len(index); {
		if index[i] == '\\' && i+1 < len(index) && index[i+1] == '$' {
			out.WriteString(index[i : i+2])
			i += 2
			continue
		}
		if index[i] != '$' {
			out.WriteByte(index[i])
			i++
			continue
		}
		name, end, ok := indexVariableReference(index, i)
		if !ok {
			out.WriteByte(index[i])
			i++
			continue
		}
		variableName, fallback, hasFallback := strings.Cut(name, "??")
		variableName = strings.TrimSpace(variableName)
		expression := variableName
		if !strings.HasPrefix(expression, "$") {
			expression = "$" + expression
		}
		value := stringifyIndexValue(base.SnapshotValue(snapshot, expression))
		if value == "" && hasFallback {
			value = strings.TrimSpace(fallback)
		}
		out.WriteString(value)
		i = end
	}
	return out.String()
}

func elasticsearchLogFields(r *http.Request, logFormat map[string]string) map[string]any {
	fields := apisixlog.GetFields(r, logFormat)
	for key, value := range logFormat {
		switch value {
		case "$host":
			fields[key] = r.Host
		case "$remote_addr":
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err == nil {
				fields[key] = host
			}
		}
	}
	return fields
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, _ int) (int, error) {
	endpoint := p.endpointAddr()
	if endpoint == "" {
		return 0, nil
	}
	body, err := p.bulkBodyEntries(entries)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal Elasticsearch bulk body: %w", err)
	}
	firstFail := 0
	sendCtx, cancel := context.WithTimeout(ctx, time.Duration(p.config.Timeout)*time.Second)
	defer cancel()
	err = p.withClient(endpoint, func(client *elasticsearch.BaseClient) error {
		resp, sendErr := (esapi.BulkRequest{
			Body:   bytes.NewReader(body),
			Header: http.Header{"Content-Type": []string{"application/x-ndjson"}},
		}).Do(sendCtx, client)
		if sendErr != nil {
			return fmt.Errorf("failed to send log message: %w", sendErr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.IsError() {
			return fmt.Errorf("failed to send log message: elasticsearch returned status %s", resp.Status())
		}
		var resultErr error
		firstFail, resultErr = p.bulkResultFailure(resp.Body)
		if resultErr != nil {
			return fmt.Errorf("failed to deliver Elasticsearch bulk: %w", resultErr)
		}
		return nil
	})
	if err != nil {
		return firstFail, err
	}
	return firstFail, nil
}

// bulkResultFailure inspects a 2xx bulk response and returns the first failing
// item as a 1-based index. A bulk operation is failing when its status is 300
// or higher or it carries an error payload. A response that reports errors but
// contains no decodable failing item is treated as a malformed result.
func (p *Plugin) bulkResultFailure(body io.Reader) (int, error) {
	var result struct {
		Errors bool                         `json:"errors"`
		Items  []map[string]json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return 1, fmt.Errorf("failed to decode Elasticsearch bulk result: %w", err)
	}
	for index, item := range result.Items {
		for _, operationJSON := range item {
			var operation struct {
				Status int             `json:"status"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(operationJSON, &operation); err != nil {
				return index + 1, fmt.Errorf("failed to decode Elasticsearch bulk item %d: %w", index+1, err)
			}
			if operation.Status >= 300 || bulkItemError(operation.Error) {
				return index + 1, fmt.Errorf("elasticsearch bulk item %d failed: status %d", index+1, operation.Status)
			}
		}
	}
	if result.Errors {
		return 1, fmt.Errorf("elasticsearch bulk result reported errors without a failing item")
	}
	return 0, nil
}

func bulkItemError(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func (p *Plugin) endpointAddr() string {
	if p.config.EndpointAddr != "" {
		return p.config.EndpointAddr
	}
	if len(p.config.EndpointAddrs) == 0 {
		return ""
	}
	return p.config.EndpointAddrs[randomEndpointIndex(len(p.config.EndpointAddrs))]
}

func (p *Plugin) withClient(
	endpoint string, use func(*elasticsearch.BaseClient) error,
) error {
	if use == nil {
		return secret.ErrCredentialUnavailable
	}
	p.secretMu.RLock()
	defer p.secretMu.RUnlock()
	if p.stopped.Load() || !p.credentialsInstalled() {
		return secret.ErrCredentialUnavailable
	}

	key := esClientKey{endpoint: endpoint}
	if p.password != nil {
		key.passwordDigest = p.password.Digest()
	}
	if p.authorization != nil {
		key.authorizationDigest = p.authorization.Digest()
	}

	client, err := p.clientForEndpoint(key)
	if err != nil {
		return err
	}
	return use(client)
}

func (p *Plugin) clientForEndpoint(key esClientKey) (*elasticsearch.BaseClient, error) {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.clients == nil {
		p.clients = make(map[esClientKey]*esClientRef)
	}
	if ref := p.clients[key]; ref != nil {
		return ref.client, nil
	}

	client, credentials, release, err := p.newPluginOwnedClient(key.endpoint)
	if err != nil {
		return nil, err
	}
	p.clients[key] = &esClientRef{
		client: client, credentials: credentials, release: release,
	}
	return client, nil
}

func (p *Plugin) useCredentialPlaintext(use func(password, authorization string) error) error {
	if use == nil {
		return secret.ErrCredentialUnavailable
	}
	useAuthorization := func(password string) error {
		if p.authorization != nil {
			return p.authorization.Use(func(authorization string) error {
				return use(password, authorization)
			})
		}
		return use(password, "")
	}
	if p.password != nil {
		return p.password.Use(useAuthorization)
	}
	return useAuthorization("")
}

func (p *Plugin) newPluginOwnedClient(
	endpoint string,
) (*elasticsearch.BaseClient, *elasticsearchCredentialTransport, func(), error) {
	clientUID := shared.NewConfigUID()
	clientUID.Add(p.config.Timeout, *p.config.SslVerify)
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name+"-transport", clientUID),
		func() (any, error) {
			return newElasticsearchNeutralTransport(
				time.Duration(p.config.Timeout)*time.Second, *p.config.SslVerify,
			), nil
		},
		func(v any) { v.(*http.Transport).CloseIdleConnections() },
	)
	if err != nil {
		return nil, nil, nil, err
	}
	_, exactAuthorization := p.config.Headers["Authorization"]
	transport := &elasticsearchCredentialTransport{
		owner: p, transport: value.(*http.Transport), override: exactAuthorization,
	}
	client, err := elasticsearch.NewBaseClient(elasticsearch.Config{
		Addresses: []string{endpoint},
		Header:    headerFromMapWithoutExactAuthorization(p.config.Headers),
		Transport: transport,
	})
	if err != nil {
		transport.destroy()
		release()
		return nil, nil, nil, err
	}
	return client, transport, release, nil
}

func newElasticsearchNeutralTransport(timeout time.Duration, sslVerify bool) *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		ResponseHeaderTimeout: timeout,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: !sslVerify},
	}
}

func (transport *elasticsearchCredentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if transport == nil || transport.owner == nil || transport.transport == nil {
		return nil, secret.ErrCredentialUnavailable
	}
	var response *http.Response
	err := transport.owner.useCredentialPlaintext(func(password, authorization string) error {
		var derived []byte
		if authorization != "" {
			derived = []byte(authorization)
		} else if transport.owner.config.Auth != nil {
			derived = basicAuthorization(transport.owner.config.Auth.Username, password)
		}
		defer clearBytes(derived)
		if transport.afterDerive != nil {
			transport.afterDerive(derived)
		}

		request := req.Clone(req.Context())
		request.Header = req.Header.Clone()
		if len(derived) > 0 &&
			(transport.override || request.Header.Get("Authorization") == "") {
			request.Header.Set("Authorization", string(derived))
		}
		var roundTripErr error
		response, roundTripErr = transport.transport.RoundTrip(request)
		request.Header.Del("Authorization")
		if response != nil && response.Request == request {
			response.Request.Header.Del("Authorization")
		}
		return roundTripErr
	})
	return response, err
}

func (transport *elasticsearchCredentialTransport) destroy() {
	if transport == nil {
		return
	}
	transport.owner = nil
	transport.transport = nil
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func basicAuthorization(username, password string) []byte {
	raw := []byte(username + ":" + password)
	encoded := make([]byte, len("Basic ")+base64.StdEncoding.EncodedLen(len(raw)))
	copy(encoded, "Basic ")
	base64.StdEncoding.Encode(encoded[len("Basic "):], raw)
	clearBytes(raw)
	return encoded
}

func (p *Plugin) QuiesceGenerationTasks() { p.Stop() }

func (p *Plugin) Stop() {
	p.cleanupOnce.Do(func() {
		if p.stopBeforeLock != nil {
			p.stopBeforeLock()
		}
		p.stopped.Store(true)

		cleanup := func() {
			p.secretMu.Lock()
			p.clientMu.Lock()
			refs := make([]*esClientRef, 0, len(p.clients))
			for _, ref := range p.clients {
				refs = append(refs, ref)
			}
			p.clients = nil
			p.clientMu.Unlock()

			for _, ref := range refs {
				if err := ref.client.Close(context.Background()); err != nil {
					logger.Errorf("failed to close Elasticsearch client: %s", err)
				}
				ref.credentials.destroy()
				ref.release()
			}
			if p.password != nil {
				*p.password = secret.Value{}
				p.password = nil
			}
			if p.authorization != nil {
				*p.authorization = secret.Value{}
				p.authorization = nil
			}
			p.secretMu.Unlock()
		}
		p.secretMu.RLock()
		processor := p.BatchProcessor
		p.secretMu.RUnlock()
		if processor != nil {
			processor.StopWithCleanup(cleanup)
		} else {
			cleanup()
		}
	})
}

func (p *Plugin) bulkBodyEntries(entries []map[string]any) ([]byte, error) {
	var body bytes.Buffer
	for _, entry := range entries {
		entryBody, err := p.bulkBodyEntry(entry)
		if err != nil {
			return nil, err
		}
		body.Write(entryBody)
	}
	return body.Bytes(), nil
}

func (p *Plugin) bulkBodyEntry(log map[string]any) ([]byte, error) {
	index := p.config.Field.Index
	if resolvedIndex, ok := log[elasticsearchIndexField].(string); ok && resolvedIndex != "" {
		index = resolvedIndex
	}
	action := map[string]any{
		"index": map[string]any{
			"_index": index,
		},
	}
	if version := p.elasticsearchVersion(); version == "6" || version == "5" {
		indexAction := action["index"].(map[string]any)
		indexAction["_type"] = "_doc"
	}

	actionLine, err := json.Marshal(action)
	if err != nil {
		return nil, err
	}
	logLine, err := json.Marshal(elasticsearchDocument(log))
	if err != nil {
		return nil, err
	}

	body := make([]byte, 0, len(actionLine)+len(logLine)+2)
	body = append(body, actionLine...)
	body = append(body, '\n')
	body = append(body, logLine...)
	body = append(body, '\n')
	return body, nil
}

func (p *Plugin) fetchAndUpdateVersion(client *elasticsearch.BaseClient) {
	p.versionMu.Lock()
	defer p.versionMu.Unlock()
	if p.esVersion != "" {
		return
	}

	version, err := p.getMajorVersion(client)
	if err != nil {
		logger.Errorf("failed to get Elasticsearch version: %s", err)
		return
	}
	p.esVersion = version
}

func (p *Plugin) elasticsearchVersion() string {
	p.versionMu.Lock()
	defer p.versionMu.Unlock()
	return p.esVersion
}

func (p *Plugin) getMajorVersion(client *elasticsearch.BaseClient) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.config.Timeout)*time.Second)
	defer cancel()
	resp, err := (esapi.InfoRequest{}).Do(ctx, client)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.IsError() {
		return "", fmt.Errorf("server returned status: %s", resp.Status())
	}

	var body struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Version.Number == "" {
		return "", fmt.Errorf("failed to get version from response body")
	}
	major, _, found := strings.Cut(body.Version.Number, ".")
	if !found || major == "" {
		return "", fmt.Errorf("invalid version format: %s", body.Version.Number)
	}
	return major, nil
}

func elasticsearchDocument(log map[string]any) map[string]any {
	if _, ok := log[elasticsearchIndexField]; !ok {
		return log
	}

	doc := make(map[string]any, len(log)-1)
	for key, value := range log {
		if key == elasticsearchIndexField {
			continue
		}
		doc[key] = value
	}
	return doc
}

func resolveIndexVars(index string, r *http.Request) string {
	index = replaceIndexTimeVars(index)
	index = resolveIndexVariableReferences(index, r)
	return index
}

func resolveIndexVariableReferences(index string, r *http.Request) string {
	var out strings.Builder
	for i := 0; i < len(index); {
		if index[i] == '\\' && i+1 < len(index) && index[i+1] == '$' {
			out.WriteString(index[i : i+2])
			i += 2
			continue
		}
		if index[i] != '$' {
			out.WriteByte(index[i])
			i++
			continue
		}

		name, end, ok := indexVariableReference(index, i)
		if !ok {
			out.WriteByte(index[i])
			i++
			continue
		}
		if value, found := indexVariableValue(strings.TrimSpace(name), r); found {
			out.WriteString(value)
		} else if _, fallback, hasFallback := strings.Cut(name, "??"); hasFallback {
			out.WriteString(strings.TrimSpace(fallback))
		}
		i = end
	}
	return out.String()
}

func indexVariableReference(index string, start int) (name string, end int, ok bool) {
	if start+1 >= len(index) {
		return "", start, false
	}
	if index[start+1] == '{' {
		close := strings.IndexByte(index[start+2:], '}')
		if close < 0 {
			return "", start, false
		}
		end = start + 3 + close
		name = index[start+2 : end-1]
		if name == "" {
			return "", start, false
		}
		return name, end, true
	}
	end = start + 1
	for end < len(index) && (index[end] == '_' || index[end] == '.' ||
		index[end] >= 'a' && index[end] <= 'z' ||
		index[end] >= 'A' && index[end] <= 'Z' ||
		index[end] >= '0' && index[end] <= '9') {
		end++
	}
	if end == start+1 {
		return "", start, false
	}
	return index[start+1 : end], end, true
}

func indexVariableValue(name string, r *http.Request) (string, bool) {
	variableName, _, _ := strings.Cut(name, "??")
	variableName = strings.TrimSpace(variableName)
	if argument, ok := strings.CutPrefix(variableName, "arg_"); ok {
		values, found := r.URL.Query()[argument]
		if !found || len(values) == 0 {
			return "", false
		}
		return values[0], true
	}
	key := "$" + variableName
	if _, ok := variable.NginxVars[key]; ok {
		return stringifyIndexValue(apisixlog.GetField(r, key)), true
	}
	if _, ok := variable.ApisixVars[key]; ok {
		if key != "$matched_uri" {
			value, found := apisixctx.GetApisixVars(r)[key]
			if !found {
				return "", false
			}
			return resolvedIndexVariableValue(value)
		}
		return resolvedIndexVariableValue(apisixlog.GetField(r, key))
	}
	if _, ok := variable.RequestVars[key]; ok {
		value, found := apisixctx.GetRequestVars(r)[key]
		if !found {
			return "", false
		}
		return resolvedIndexVariableValue(value)
	}
	return "", false
}

func resolvedIndexVariableValue(value any) (string, bool) {
	if value == nil {
		return "", false
	}
	return stringifyIndexValue(value), true
}

func replaceIndexTimeVars(index string) string {
	var out strings.Builder
	for i := 0; i < len(index); i++ {
		if index[i] != '{' || (i > 0 && index[i-1] == '$') {
			out.WriteByte(index[i])
			continue
		}

		end := strings.IndexByte(index[i+1:], '}')
		if end < 0 {
			out.WriteByte(index[i])
			continue
		}

		format := index[i+1 : i+1+end]
		out.WriteString(time.Now().Format(strftimeToGo(format)))
		i += end + 1
	}
	return out.String()
}

func strftimeToGo(format string) string {
	replacer := strings.NewReplacer(
		"%%", "%",
		"%Y", "2006",
		"%y", "06",
		"%m", "01",
		"%d", "02",
		"%H", "15",
		"%M", "04",
		"%S", "05",
		"%F", "2006-01-02",
		"%T", "15:04:05",
		"%z", "-0700",
		"%Z", "MST",
		"%b", "Jan",
		"%B", "January",
		"%a", "Mon",
		"%A", "Monday",
	)
	return replacer.Replace(format)
}

func stringifyIndexValue(value any) string {
	if value == nil {
		return ""
	}
	if str, ok := value.(string); ok {
		return str
	}
	bytes, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(bytes)
}

func headerFromMapWithoutExactAuthorization(headers map[string]string) http.Header {
	if len(headers) == 0 {
		return nil
	}
	out := make(http.Header, len(headers))
	for key, value := range headers {
		if key == "Authorization" {
			continue
		}
		out.Set(key, value)
	}
	return out
}
