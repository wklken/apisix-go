package google_cloud_logging

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/util"
	"golang.org/x/oauth2"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client

	lifecycleMu     sync.RWMutex
	resolvedAuth    *AuthConfig
	secretsPrepared bool
	stopped         atomic.Bool

	requestTimeout time.Duration

	tokenMu      sync.Mutex
	accessToken  string
	tokenType    string
	tokenExpires time.Time

	clientRelease func()
}

const (
	priority = 407
	name     = "google-cloud-logging"

	defaultTokenURI   = "https://oauth2.googleapis.com/token"
	defaultEntriesURI = "https://logging.googleapis.com/v2/entries:write"
	defaultLogID      = "apisix.apache.org%2Flogs"

	jwtBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	defaultGoogleCloudLoggingTimeout = 10 * time.Second
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

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "log_format": {
      "type": "object"
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
	p.MetadataSchema = metadataSchema

	p.InitLogger(p.Send)

	return nil
}

// MaterializeScopedSecrets admits only the manifest-owned inline private key.
// File-backed service-account contents remain owned by auth_file handling.
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
	if p.config.AuthConfig == nil {
		p.secretsPrepared = true
		return nil
	}

	value, err := access.Materialize(
		ctx, "auth_config.private_key", p.config.AuthConfig.PrivateKey,
	)
	if err != nil {
		return googlePrivateKeyUnavailable()
	}
	var resolvedAuth *AuthConfig
	if err := value.Use(func(privateKey string) error {
		var buildErr error
		resolvedAuth, buildErr = p.buildResolvedInlineAuth(privateKey)
		return buildErr
	}); err != nil {
		return googlePrivateKeyUnavailable()
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return googlePrivateKeyUnavailable()
	}

	p.resolvedAuth = resolvedAuth
	p.config.AuthConfig.PrivateKey = descriptor.String()
	p.secretsPrepared = true
	return nil
}

func (p *Plugin) buildResolvedInlineAuth(privateKey string) (*AuthConfig, error) {
	if err := validateGooglePrivateKey(privateKey); err != nil {
		return nil, err
	}
	auth := *p.config.AuthConfig
	auth.PrivateKey = privateKey
	auth.Scope = append([]string(nil), p.config.AuthConfig.Scope...)
	auth.Scopes = append([]string(nil), p.config.AuthConfig.Scopes...)
	p.applyAuthDefaults(&auth)
	return &auth, nil
}

func validateGooglePrivateKey(privateKey string) error {
	encoded := []byte(privateKey)
	defer clear(encoded)
	keyBytes := encoded
	if block, _ := pem.Decode(encoded); block != nil {
		keyBytes = block.Bytes
		defer clear(keyBytes)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBytes)
	if err != nil {
		parsed, err = x509.ParsePKCS1PrivateKey(keyBytes)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
	}
	if _, ok := parsed.(*rsa.PrivateKey); !ok {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func googlePrivateKeyUnavailable() error {
	return fmt.Errorf("%s auth_config.private_key: %w", name, secret.ErrCredentialUnavailable)
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	if len(p.LogFormat) > 0 {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			_ = p.enqueueLogIfRunning(apisixlog.GetFields(r, p.LogFormat))
		})
	}

	fn := func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}

		_ = p.enqueueLogIfRunning(p.defaultLogFields(r, recorder, time.Since(start)))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	var fields map[string]any
	if len(p.LogFormat) > 0 {
		fields = base.GetFieldsFromSnapshot(snapshot, p.LogFormat)
	} else {
		fields = googleSnapshotDefaultLogFields(snapshot)
	}
	return p.enqueueLogIfRunning(fields)
}

func (p *Plugin) enqueueLogIfRunning(fields map[string]any) error {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() {
		return base.ErrLogQueueUnavailable
	}
	if p.BatchProcessor == nil {
		return p.Fire(fields)
	}
	return p.EnqueueLog(fields)
}

func googleSnapshotDefaultLogFields(snapshot base.LogSnapshot) map[string]any {
	requestSize := snapshot.Request.ContentLength
	requestSize = max(requestSize, 0)
	latency := time.Duration(0)
	if !snapshot.Started.IsZero() && !snapshot.Finished.IsZero() {
		latency = snapshot.Finished.Sub(snapshot.Started)
	}
	remoteIP := snapshot.Request.RemoteAddr
	if host, _, err := net.SplitHostPort(remoteIP); err == nil {
		remoteIP = host
	}
	fields := map[string]any{
		defaultEntryMarker:        true,
		defaultRequestMethodField: snapshot.Request.Method,
		defaultRequestURLField:    googleSnapshotRequestURL(snapshot),
		defaultRequestSizeField:   requestSize,
		defaultStatusField:        snapshot.Outcome.Status,
		defaultResponseSizeField:  snapshot.Outcome.Bytes,
		defaultUserAgentField:     snapshot.Request.Header.Get("User-Agent"),
		defaultRemoteIPField:      remoteIP,
		defaultServerIPField:      snapshot.Request.Host,
		defaultLatencyField:       latencyString(latency),
		defaultInsertIDField:      snapshot.Request.Header.Get("X-Request-ID"),
	}
	if routeID := stringFromAny(base.SnapshotValue(snapshot, "$route_id")); routeID != "" {
		fields["route_id"] = routeID
	}
	if serviceID := stringFromAny(base.SnapshotValue(snapshot, "$service_id")); serviceID != "" {
		fields["service_id"] = serviceID
	}
	return fields
}

func googleSnapshotRequestURL(snapshot base.LogSnapshot) string {
	if parsed, err := url.Parse(snapshot.Request.URL); err == nil && parsed.IsAbs() {
		return parsed.String()
	}
	scheme := snapshot.Request.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + snapshot.Request.Host + snapshot.Request.URI
}

func latencyString(latency time.Duration) string {
	return strconv.FormatFloat(latency.Seconds(), 'f', 3, 64) + "s"
}

func (p *Plugin) PostInit() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopped.Load() || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	var metadata pluginMetadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("%s metadata decode failed: %w", name, err)
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
	if p.resolvedAuth != nil {
		p.applyAuthDefaults(p.resolvedAuth)
	}

	configUID := shared.NewConfigUID()
	if p.config.AuthConfig != nil {
		configUID.Add(p.config.AuthConfig.ClientEmail)
		configUID.Add(p.config.AuthConfig.ProjectID)
		configUID.Add(p.config.AuthConfig.TokenURI)
		configUID.Add(p.config.AuthConfig.EntriesURI)
	}
	configUID.Add(p.config.AuthFile)
	configUID.Add(p.sslVerify())

	tlsConfig, trustIdentity, err := googleCloudTLSConfig(p.sslVerify())
	if err != nil {
		return err
	}
	configUID.Add(trustIdentity)
	if p.requestTimeout <= 0 {
		p.requestTimeout = defaultGoogleCloudLoggingTimeout
	}
	client := resty.New()
	client.SetTLSClientConfig(tlsConfig)
	client.SetTimeout(p.requestTimeout)
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) { return client, nil },
		shared.CloseRestyClient,
	)
	if err != nil {
		return err
	}
	sharedClient := value.(*resty.Client)

	// Parse the immutable file-backed auth once so the delivery path does not
	// read the auth file. Inline auth was already derived during materialization.
	if p.config.AuthConfig == nil {
		if auth, err := p.resolveAuthConfig(); err == nil {
			p.resolvedAuth = auth
		}
	}

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

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
	return nil
}

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
		if p.clientRelease != nil {
			p.clientRelease()
			p.clientRelease = nil
		}
		p.client = nil
		p.BatchProcessor = nil
		if p.resolvedAuth != nil {
			p.resolvedAuth.PrivateKey = ""
			p.resolvedAuth = nil
		}
		p.tokenMu.Lock()
		p.accessToken = ""
		p.tokenType = ""
		p.tokenExpires = time.Time{}
		p.tokenMu.Unlock()
		p.secretsPrepared = false
	}
	if processor != nil {
		processor.StopWithCleanup(cleanup)
	} else {
		cleanup()
	}
}

func googleCloudTLSConfig(verify bool) (*tls.Config, string, error) {
	config := &tls.Config{InsecureSkipVerify: !verify}
	if !verify {
		return config, "insecure", nil
	}

	caFile := os.Getenv("SSL_CERT_FILE")
	if caFile == "" {
		return config, "verified:system", nil
	}
	certificate, err := os.ReadFile(caFile)
	if err != nil {
		return nil, "", fmt.Errorf("read google-cloud-logging trusted certificate file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return nil, "", fmt.Errorf("parse google-cloud-logging trusted certificate file %q", caFile)
	}
	config.RootCAs = roots
	trustHash := sha256.Sum256(certificate)
	return config, fmt.Sprintf("verified:file:%x", trustHash), nil
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, _ int) (int, error) {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() || p.client == nil {
		return 0, secret.ErrCredentialUnavailable
	}
	auth, err := p.authConfigLocked()
	if err != nil {
		return 0, fmt.Errorf("failed to load google-cloud-logging auth config: %w", err)
	}

	accessToken, tokenType, err := p.accessTokenFor(ctx, auth)
	if err != nil {
		return 0, fmt.Errorf("failed to get google-cloud-logging oauth token: %w", err)
	}
	if tokenType == "" {
		tokenType = "Bearer"
	}

	googleEntries := make([]googleLogEntry, 0, len(entries))
	for _, entry := range entries {
		googleEntries = append(googleEntries, p.buildEntryForProject(entry, auth.ProjectID))
	}
	body := map[string]any{
		"entries":        googleEntries,
		"partialSuccess": false,
	}
	resp, err := p.client.R().
		SetContext(ctx).
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
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	return p.authConfigLocked()
}

func (p *Plugin) authConfigLocked() (*AuthConfig, error) {
	if p.stopped.Load() {
		return nil, secret.ErrCredentialUnavailable
	}
	if p.resolvedAuth != nil {
		return cloneGoogleAuth(p.resolvedAuth), nil
	}
	return p.resolveAuthConfig()
}

func cloneGoogleAuth(source *AuthConfig) *AuthConfig {
	if source == nil {
		return nil
	}
	auth := *source
	auth.Scope = append([]string(nil), source.Scope...)
	auth.Scopes = append([]string(nil), source.Scopes...)
	return &auth
}

// resolveAuthConfig builds the effective auth config from auth_config or the
// auth_file, applying defaults. It performs file I/O when the auth file is
// used, so it is called once at initialization on the cached path.
func (p *Plugin) resolveAuthConfig() (*AuthConfig, error) {
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
	defer clear(data)
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

func (a *AuthConfig) scopes() []string {
	if len(a.Scopes) > 0 {
		return a.Scopes
	}
	return a.Scope
}

func (p *Plugin) accessTokenFor(ctx context.Context, auth *AuthConfig) (string, string, error) {
	p.tokenMu.Lock()
	if p.accessToken != "" && time.Now().Before(p.tokenExpires.Add(-tokenRefreshSkew)) {
		accessToken := p.accessToken
		tokenType := p.tokenType
		p.tokenMu.Unlock()
		return accessToken, tokenType, nil
	}
	p.tokenMu.Unlock()

	// The network refresh runs outside the mutex so one slow token endpoint
	// does not serialize every concurrent batch.
	token, err := p.fetchAccessToken(ctx, auth)
	if err != nil {
		return "", "", err
	}

	p.tokenMu.Lock()
	p.accessToken = token.AccessToken
	p.tokenType = token.TokenType
	p.tokenExpires = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	accessToken := p.accessToken
	tokenType := p.tokenType
	p.tokenMu.Unlock()
	return accessToken, tokenType, nil
}

func (p *Plugin) fetchAccessToken(ctx context.Context, auth *AuthConfig) (tokenResponse, error) {
	source := googleTokenSource(ctx, auth, p.client.GetClient())
	if source == nil {
		return tokenResponse{}, errors.New("failed to build Google token source")
	}
	token, err := source.Token()
	if err != nil {
		return tokenResponse{}, err
	}
	expiresIn := 0
	if !token.Expiry.IsZero() {
		expiresIn = int(time.Until(token.Expiry).Seconds())
	}
	return tokenResponse{
		AccessToken: token.AccessToken,
		TokenType:   token.TokenType,
		ExpiresIn:   expiresIn,
	}, nil
}

func googleTokenSource(ctx context.Context, auth *AuthConfig, client *http.Client) oauth2.TokenSource {
	rawJSON, err := json.Marshal(map[string]any{
		"type":         "service_account",
		"client_email": auth.ClientEmail,
		"private_key":  auth.PrivateKey,
		"project_id":   auth.ProjectID,
		"token_uri":    auth.TokenURI,
	})
	if err != nil {
		return nil
	}
	defer clear(rawJSON)
	source, err := ai_auth.NewGoogleTokenSource(ctx, rawJSON, auth.scopes(), client)
	if err != nil {
		return nil
	}
	return source
}

func (p *Plugin) buildEntry(log map[string]any) googleLogEntry {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	projectID := ""
	if p.resolvedAuth != nil {
		projectID = p.resolvedAuth.ProjectID
	} else if p.config.AuthConfig != nil {
		projectID = p.config.AuthConfig.ProjectID
	}
	return p.buildEntryForProject(log, projectID)
}

func (p *Plugin) buildEntryForProject(log map[string]any, projectID string) googleLogEntry {
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
		defaultRequestSizeField:   util.RequestSize(r),
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
