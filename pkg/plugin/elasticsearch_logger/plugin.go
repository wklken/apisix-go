package elasticsearch_logger

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/data_encryption"
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

	p.FireChan = make(chan map[string]any, 1000)
	p.AsyncBlock = true

	p.SendFunc = p.Send

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Auth != nil {
		keyring, enabled := data_encryption.Keyring()
		resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(p.config.Auth.Password)
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
	if len(p.config.LogFormat) == 0 {
		p.LogFormat = metadata.LogFormat
	} else {
		p.LogFormat = p.config.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              name,
		BatchMaxSize:      p.config.BatchMaxSize,
		MaxRetryCount:     p.config.MaxRetryCount,
		RetryDelay:        time.Duration(p.config.RetryDelay) * time.Second,
		BufferDuration:    time.Duration(p.config.BufferDuration) * time.Second,
		InactiveTimeout:   time.Duration(p.config.InactiveTimeout) * time.Second,
		MaxPendingEntries: p.config.MaxPendingEntries,
		RouteID:           p.RouteID,
		ServerAddr:        p.ServerAddr,
	}, p.SendBatch)

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var requestBody string
		if p.config.IncludeReqBody && base.ExprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := base.ReadAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *base.ResponseRecorder
		if p.config.IncludeRespBody {
			recorder = base.NewResponseRecorder(w, p.config.MaxRespBodyBytes)
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		status := 0
		if recorder != nil {
			status = recorder.StatusCode()
		}

		logFields := apisixlog.GetFields(r, p.LogFormat)
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.Body()
		}
		logFields[elasticsearchIndexField] = resolveIndexVars(p.config.Field.Index, r)
		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch([]map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, _ int) (int, error) {
	endpoint := p.endpointAddr()
	if endpoint == "" {
		return 0, nil
	}
	client, err := p.clientForEndpoint(endpoint)
	if err != nil {
		return 0, fmt.Errorf("failed to create Elasticsearch client: %w", err)
	}
	p.fetchAndUpdateVersion(endpoint)

	body, err := p.bulkBodyEntries(entries)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal Elasticsearch bulk body: %w", err)
	}

	resp, err := client.Bulk(
		bytes.NewReader(body),
		client.Bulk.WithTimeout(time.Duration(p.config.Timeout)*time.Second),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to send log message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.IsError() {
		return 0, fmt.Errorf("failed to send log message: elasticsearch returned status %s", resp.Status())
	}
	return 0, nil
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

func (p *Plugin) clientForEndpoint(endpoint string) (*elasticsearch.Client, error) {
	username := ""
	password := ""
	if p.config.Auth != nil {
		username = p.config.Auth.Username
		password = p.config.Auth.Password
	}

	c, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{endpoint},
		Username:  username,
		Password:  password,
		Header:    headerFromMap(p.config.Headers),
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: time.Duration(p.config.Timeout) * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: time.Duration(p.config.Timeout) * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: !*p.config.SslVerify},
		},
	})
	if err != nil {
		return nil, err
	}

	clientUID := shared.NewConfigUID()
	clientUID.Add(endpoint, username, password, p.config.Headers, p.config.Timeout, *p.config.SslVerify)
	return shared.LoadOrStoreClient(name, clientUID, c).(*elasticsearch.Client), nil
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
	if p.config.Field.Type != nil && *p.config.Field.Type != "" {
		action["index"].(map[string]any)["_type"] = *p.config.Field.Type
	} else if version := p.elasticsearchVersion(); version == "6" || version == "5" {
		action["index"].(map[string]any)["_type"] = "_doc"
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

func (p *Plugin) fetchAndUpdateVersion(endpoint string) {
	p.versionMu.Lock()
	defer p.versionMu.Unlock()
	if p.esVersion != "" {
		return
	}

	version, err := p.getMajorVersion(endpoint)
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

func (p *Plugin) getMajorVersion(endpoint string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if p.config.Auth != nil {
		req.SetBasicAuth(p.config.Auth.Username, p.config.Auth.Password)
	}
	for key, value := range p.config.Headers {
		req.Header.Set(key, value)
	}

	client := &http.Client{
		Timeout: time.Duration(p.config.Timeout) * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: time.Duration(p.config.Timeout) * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: time.Duration(p.config.Timeout) * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: !*p.config.SslVerify},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server returned status: %d", resp.StatusCode)
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
	for _, key := range sortedVariableKeys() {
		value := apisixlog.GetField(r, key)
		index = strings.ReplaceAll(index, key, stringifyIndexValue(value))
	}
	return index
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

func sortedVariableKeys() []string {
	keys := make([]string, 0, len(variable.NginxVars)+len(variable.ApisixVars)+len(variable.RequestVars))
	for key := range variable.NginxVars {
		keys = append(keys, key)
	}
	for key := range variable.ApisixVars {
		keys = append(keys, key)
	}
	for key := range variable.RequestVars {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j])
	})
	return keys
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
