package elasticsearch_logger

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
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
	clients  map[string]*esClientRef
}

type esClientRef struct {
	client  *elasticsearch.BaseClient
	release func()
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

	p.InitLogger(p.Send)

	return nil
}

func (p *Plugin) PostInit() error {
	if !p.DataEncryption().Configured() {
		return errors.New("data-encryption resolver is required")
	}
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}
	if p.config.Auth != nil {
		resolved, err := p.DataEncryption().ResolveForContext(
			p.config.Auth.Password,
			"elasticsearch-logger.auth.password",
		)
		if err != nil {
			return fmt.Errorf("elasticsearch-logger auth.password: %w", err)
		}
		p.config.Auth.Password = resolved
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

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
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

	p.BatchProcessor = base.NewBatchProcessor(name, base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.SendBatch)

	// Version detection runs once per stable config at initialization, reusing
	// the pooled client instead of building a transport per attempt.
	if endpoint := p.endpointAddr(); endpoint != "" {
		client, err := p.clientForEndpoint(endpoint)
		if err != nil {
			logger.Errorf("failed to create Elasticsearch client: %s", err)
		} else {
			p.fetchAndUpdateVersion(client)
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
	client, err := p.clientForEndpoint(endpoint)
	if err != nil {
		return 0, fmt.Errorf("failed to create Elasticsearch client: %w", err)
	}

	body, err := p.bulkBodyEntries(entries)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal Elasticsearch bulk body: %w", err)
	}

	resp, err := (esapi.BulkRequest{
		Body:    bytes.NewReader(body),
		Header:  http.Header{"Content-Type": []string{"application/x-ndjson"}},
		Timeout: time.Duration(p.config.Timeout) * time.Second,
	}).Do(ctx, client)
	if err != nil {
		return 0, fmt.Errorf("failed to send log message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.IsError() {
		return 0, fmt.Errorf("failed to send log message: elasticsearch returned status %s", resp.Status())
	}
	firstFail, err := p.bulkResultFailure(resp.Body)
	if err != nil {
		return firstFail, fmt.Errorf("failed to deliver Elasticsearch bulk: %w", err)
	}
	return 0, nil
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

func (p *Plugin) clientForEndpoint(endpoint string) (*elasticsearch.BaseClient, error) {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.clients == nil {
		p.clients = make(map[string]*esClientRef)
	}
	if ref := p.clients[endpoint]; ref != nil {
		return ref.client, nil
	}

	username := ""
	password := ""
	if p.config.Auth != nil {
		username = p.config.Auth.Username
		password = p.config.Auth.Password
	}

	c, err := newElasticsearchClient(
		endpoint,
		username,
		password,
		p.config.Headers,
		time.Duration(p.config.Timeout)*time.Second,
		*p.config.SslVerify,
	)
	if err != nil {
		return nil, err
	}

	clientUID := shared.NewConfigUID()
	clientUID.Add(endpoint, username, password, p.config.Headers, p.config.Timeout, *p.config.SslVerify)
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, clientUID),
		func() (any, error) { return c, nil },
		func(v any) { _ = v.(*elasticsearch.BaseClient).Close(context.Background()) },
	)
	if err != nil {
		return nil, err
	}
	client := value.(*elasticsearch.BaseClient)
	p.clients[endpoint] = &esClientRef{client: client, release: release}
	return client, nil
}

func newElasticsearchClient(
	endpoint, username, password string,
	headers map[string]string,
	timeout time.Duration,
	sslVerify bool,
) (*elasticsearch.BaseClient, error) {
	return elasticsearch.NewBaseClient(elasticsearch.Config{
		Addresses: []string{endpoint},
		Username:  username,
		Password:  password,
		Header:    headerFromMap(headers),
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: timeout,
			}).DialContext,
			ResponseHeaderTimeout: timeout,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: !sslVerify},
		},
	})
}

func (p *Plugin) Stop() {
	p.StopWithCleanup(func() {
		p.clientMu.Lock()
		refs := make([]*esClientRef, 0, len(p.clients))
		for _, ref := range p.clients {
			refs = append(refs, ref)
		}
		p.clients = nil
		p.clientMu.Unlock()

		for _, ref := range refs {
			ref.release()
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

func headerFromMap(headers map[string]string) http.Header {
	if len(headers) == 0 {
		return nil
	}
	out := make(http.Header, len(headers))
	for key, value := range headers {
		out.Set(key, value)
	}
	return out
}
