package google_cloud_logging

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client

	tokenMu      sync.Mutex
	accessToken  string
	tokenType    string
	tokenExpires time.Time
}

const (
	priority = 407
	name     = "google-cloud-logging"

	defaultTokenURI   = "https://oauth2.googleapis.com/token"
	defaultEntriesURI = "https://logging.googleapis.com/v2/entries:write"
	defaultLogID      = "apisix.apache.org%2Flogs"

	jwtBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

const (
	defaultEntryMarker = "__google_cloud_logging_default_entry"

	defaultRequestMethodField = "request_method"
	defaultRequestURLField    = "request_url"
	defaultRequestSizeField   = "request_size"
	defaultStatusField        = "status"
	defaultResponseSizeField  = "response_size"
	defaultUserAgentField     = "user_agent"
	defaultRemoteIPField      = "remote_ip"
	defaultServerIPField      = "server_ip"
	defaultLatencyField       = "latency"
	defaultInsertIDField      = "insert_id"
)

const tokenRefreshSkew = time.Minute

var defaultScopes = []string{
	"https://www.googleapis.com/auth/logging.read",
	"https://www.googleapis.com/auth/logging.write",
	"https://www.googleapis.com/auth/logging.admin",
	"https://www.googleapis.com/auth/cloud-platform",
}

const schema = `
{
  "type": "object",
  "properties": {
    "auth_config": {
      "type": "object",
      "properties": {
        "client_email": {
          "type": "string"
        },
        "private_key": {
          "type": "string"
        },
        "project_id": {
          "type": "string"
        },
        "token_uri": {
          "type": "string",
          "default": "https://oauth2.googleapis.com/token"
        },
        "scope": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "minItems": 1
        },
        "scopes": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "minItems": 1
        },
        "entries_uri": {
          "type": "string",
          "default": "https://logging.googleapis.com/v2/entries:write"
        }
      },
      "required": ["client_email", "private_key", "project_id", "token_uri"]
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "auth_file": {
      "type": "string"
    },
    "resource": {
      "type": "object",
      "properties": {
        "type": {
          "type": "string"
        },
        "labels": {
          "type": "object"
        }
      },
      "default": {
        "type": "global"
      },
      "required": ["type"]
    },
    "log_id": {
      "type": "string",
      "default": "apisix.apache.org%2Flogs"
    },
    "log_format": {
      "type": "object"
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
    {"required": ["auth_config"]},
    {"required": ["auth_file"]}
  ]
}
`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type AuthConfig struct {
	ClientEmail string   `json:"client_email"`
	PrivateKey  string   `json:"private_key"`
	ProjectID   string   `json:"project_id"`
	TokenURI    string   `json:"token_uri"`
	Scope       []string `json:"scope,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
	EntriesURI  string   `json:"entries_uri,omitempty"`
}

type MonitoredResource struct {
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels,omitempty"`
}

type Config struct {
	AuthConfig *AuthConfig       `json:"auth_config,omitempty"`
	AuthFile   string            `json:"auth_file,omitempty"`
	SSLVerify  *bool             `json:"ssl_verify,omitempty"`
	Resource   MonitoredResource `json:"resource"`
	LogID      string            `json:"log_id,omitempty"`
	LogFormat  map[string]string `json:"log_format,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
}

type googleLogEntry struct {
	JSONPayload map[string]any     `json:"jsonPayload"`
	Labels      map[string]string  `json:"labels"`
	Timestamp   string             `json:"timestamp"`
	Resource    MonitoredResource  `json:"resource"`
	LogName     string             `json:"logName"`
	InsertID    string             `json:"insertId,omitempty"`
	HTTPRequest *googleHTTPRequest `json:"httpRequest,omitempty"`
}

type googleHTTPRequest struct {
	RequestMethod string `json:"requestMethod,omitempty"`
	RequestURL    string `json:"requestUrl,omitempty"`
	RequestSize   int64  `json:"requestSize,omitempty"`
	Status        int    `json:"status,omitempty"`
	ResponseSize  int64  `json:"responseSize,omitempty"`
	UserAgent     string `json:"userAgent,omitempty"`
	RemoteIP      string `json:"remoteIp,omitempty"`
	ServerIP      string `json:"serverIp,omitempty"`
	Latency       string `json:"latency,omitempty"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	size   int64
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	w.size += int64(n)
	return n, err
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

func (p *Plugin) Handler(next http.Handler) http.Handler {
	if len(p.LogFormat) > 0 {
		return p.BaseLoggerPlugin.Handler(next)
	}

	fn := func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}

		_ = p.Fire(p.defaultLogFields(r, recorder, time.Since(start)))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) PostInit() error {
	if p.config.AuthConfig != nil {
		keyring, enabled := data_encryption.Keyring()
		resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(p.config.AuthConfig.PrivateKey)
		if err != nil {
			return fmt.Errorf("google-cloud-logging auth_config.private_key: %w", err)
		}
		p.config.AuthConfig.PrivateKey = resolved
	}

	if p.config.Resource.Type == "" {
		p.config.Resource.Type = "global"
	}
	if p.config.LogID == "" {
		p.config.LogID = defaultLogID
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
	p.applyAuthDefaults(p.config.AuthConfig)

	configUID := shared.NewConfigUID()
	if p.config.AuthConfig != nil {
		configUID.Add(p.config.AuthConfig.ClientEmail)
		configUID.Add(p.config.AuthConfig.ProjectID)
		configUID.Add(p.config.AuthConfig.TokenURI)
		configUID.Add(p.config.AuthConfig.EntriesURI)
	}
	configUID.Add(p.config.AuthFile)
	configUID.Add(p.sslVerify())

	client := resty.New()
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.sslVerify()})
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
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

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch([]map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, _ int) (int, error) {
	auth, err := p.authConfig()
	if err != nil {
		return 0, fmt.Errorf("failed to load google-cloud-logging auth config: %w", err)
	}

	accessToken, tokenType, err := p.accessTokenFor(auth)
	if err != nil {
		return 0, fmt.Errorf("failed to get google-cloud-logging oauth token: %w", err)
	}
	if tokenType == "" {
		tokenType = "Bearer"
	}

	googleEntries := make([]googleLogEntry, 0, len(entries))
	for _, entry := range entries {
		googleEntries = append(googleEntries, p.buildEntry(entry))
	}
	body := map[string]any{
		"entries":        googleEntries,
		"partialSuccess": false,
	}
	resp, err := p.client.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", tokenType+" "+accessToken).
		SetBody(body).
		Post(auth.EntriesURI)
	if err != nil {
		return 0, fmt.Errorf("failed to write log to Google Cloud Logging endpoint %s: %w", auth.EntriesURI, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return 0, fmt.Errorf(
			"google Cloud Logging endpoint returned status code [%d], body [%s]",
			resp.StatusCode(),
			resp.String(),
		)
	}
	return 0, nil
}

func (p *Plugin) authConfig() (*AuthConfig, error) {
	if p.config.AuthConfig != nil {
		auth := *p.config.AuthConfig
		p.applyAuthDefaults(&auth)
		return &auth, nil
	}
	if p.config.AuthFile == "" {
		return nil, errors.New("auth_config or auth_file is required")
	}

	data, err := os.ReadFile(p.config.AuthFile)
	if err != nil {
		return nil, err
	}
	var auth AuthConfig
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, err
	}
	p.applyAuthDefaults(&auth)
	return &auth, nil
}

func (p *Plugin) applyAuthDefaults(auth *AuthConfig) {
	if auth == nil {
		return
	}
	if auth.TokenURI == "" {
		auth.TokenURI = defaultTokenURI
	}
	if auth.EntriesURI == "" {
		auth.EntriesURI = defaultEntriesURI
	}
	if len(auth.Scope) == 0 && len(auth.Scopes) == 0 {
		auth.Scope = append([]string(nil), defaultScopes...)
	}
}

func (p *Plugin) buildJWTAssertion(now time.Time) (string, error) {
	auth, err := p.authConfig()
	if err != nil {
		return "", err
	}

	header := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	}
	claims := map[string]any{
		"iss":   auth.ClientEmail,
		"sub":   auth.ClientEmail,
		"aud":   auth.TokenURI,
		"scope": strings.Join(auth.scopes(), " "),
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}

	encodedHeader, err := encodeJWTPart(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := encodeJWTPart(claims)
	if err != nil {
		return "", err
	}
	signingInput := encodedHeader + "." + encodedClaims

	privateKey, err := parsePrivateKey(auth.PrivateKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (a *AuthConfig) scopes() []string {
	if len(a.Scopes) > 0 {
		return a.Scopes
	}
	return a.Scope
}

func encodeJWTPart(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func parsePrivateKey(privateKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privateKey))
	if block == nil {
		return nil, errors.New("invalid private key PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private key is not RSA")
		}
		return rsaKey, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func (p *Plugin) accessTokenFor(auth *AuthConfig) (string, string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()

	if p.accessToken != "" && time.Now().Before(p.tokenExpires.Add(-tokenRefreshSkew)) {
		return p.accessToken, p.tokenType, nil
	}

	token, err := p.fetchAccessToken(auth)
	if err != nil {
		return "", "", err
	}
	p.accessToken = token.AccessToken
	p.tokenType = token.TokenType
	p.tokenExpires = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return p.accessToken, p.tokenType, nil
}

func (p *Plugin) fetchAccessToken(auth *AuthConfig) (tokenResponse, error) {
	assertion, err := p.buildJWTAssertion(time.Now())
	if err != nil {
		return tokenResponse{}, err
	}

	resp, err := p.client.R().
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetBody(url.Values{
			"grant_type": {jwtBearerGrantType},
			"assertion":  {assertion},
		}.Encode()).
		Post(auth.TokenURI)
	if err != nil {
		return tokenResponse{}, err
	}
	if resp.StatusCode() != http.StatusOK {
		return tokenResponse{}, errors.New(resp.String())
	}

	var token tokenResponse
	if err := json.Unmarshal(resp.Body(), &token); err != nil {
		return tokenResponse{}, err
	}
	if token.AccessToken == "" {
		return tokenResponse{}, errors.New("access_token is empty")
	}
	return token, nil
}

func (p *Plugin) buildEntry(log map[string]any) googleLogEntry {
	auth, err := p.authConfig()
	projectID := ""
	if err == nil {
		projectID = auth.ProjectID
	}

	entry := googleLogEntry{
		JSONPayload: log,
		Labels: map[string]string{
			"source": "apache-apisix-google-cloud-logging",
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Resource:  p.config.Resource,
		LogName:   "projects/" + projectID + "/logs/" + p.config.LogID,
	}
	if isDefaultEntry(log) {
		entry.JSONPayload = map[string]any{}
		if routeID := stringFromAny(log["route_id"]); routeID != "" {
			entry.JSONPayload["route_id"] = routeID
		}
		if serviceID := stringFromAny(log["service_id"]); serviceID != "" {
			entry.JSONPayload["service_id"] = serviceID
		}
		entry.InsertID = stringFromAny(log[defaultInsertIDField])
		entry.HTTPRequest = &googleHTTPRequest{
			RequestMethod: stringFromAny(log[defaultRequestMethodField]),
			RequestURL:    stringFromAny(log[defaultRequestURLField]),
			RequestSize:   int64FromAny(log[defaultRequestSizeField]),
			Status:        intFromAny(log[defaultStatusField]),
			ResponseSize:  int64FromAny(log[defaultResponseSizeField]),
			UserAgent:     stringFromAny(log[defaultUserAgentField]),
			RemoteIP:      stringFromAny(log[defaultRemoteIPField]),
			ServerIP:      stringFromAny(log[defaultServerIPField]),
			Latency:       stringFromAny(log[defaultLatencyField]),
		}
	}

	return entry
}

func (p *Plugin) sslVerify() bool {
	return p.config.SSLVerify == nil || *p.config.SSLVerify
}

func (p *Plugin) defaultLogFields(r *http.Request, recorder *responseRecorder, latency time.Duration) map[string]any {
	fields := map[string]any{
		defaultEntryMarker:        true,
		defaultRequestMethodField: r.Method,
		defaultRequestURLField:    requestURL(r),
		defaultRequestSizeField:   requestSize(r),
		defaultStatusField:        recorder.status,
		defaultResponseSizeField:  recorder.size,
		defaultUserAgentField:     r.UserAgent(),
		defaultRemoteIPField:      base.RemoteIP(r.RemoteAddr),
		defaultServerIPField:      r.Host,
		defaultLatencyField:       strconv.FormatFloat(latency.Seconds(), 'f', 3, 64) + "s",
		defaultInsertIDField:      r.Header.Get("X-Request-ID"),
	}
	if routeID := stringFromAny(apisixlog.GetField(r, "$route_id")); routeID != "" {
		fields["route_id"] = routeID
	}
	if serviceID := stringFromAny(apisixlog.GetField(r, "$service_id")); serviceID != "" {
		fields["service_id"] = serviceID
	}
	return fields
}

func requestURL(r *http.Request) string {
	scheme := "http"
	host := r.Host
	if r.URL.Scheme != "" {
		scheme = r.URL.Scheme
	}
	if r.TLS != nil {
		scheme = "https"
	}
	if r.URL.Host != "" {
		host = r.URL.Host
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

func requestSize(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}

func isDefaultEntry(log map[string]any) bool {
	marker, _ := log[defaultEntryMarker].(bool)
	return marker
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func intFromAny(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		parsed, _ := strconv.Atoi(v)
		return parsed
	default:
		return 0
	}
}

func int64FromAny(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		parsed, _ := strconv.ParseInt(v, 10, 64)
		return parsed
	default:
		return 0
	}
}
