package openwhisk

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/httpclient"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client

	lifecycleMu     sync.RWMutex
	serviceToken    secret.Value
	serviceTokenSet bool
	retired         bool

	// testLifecycleHook is a package-local synchronization seam for lifecycle
	// tests; it is nil in production.
	testLifecycleHook func(string)
}

const (
	priority = -1901
	name     = "openwhisk"

	lifecycleBeforeStopWait = "before-stop-wait"
)

const schema = `
{
  "type": "object",
  "properties": {
    "api_host": {
      "type": "string"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "service_token": {
      "type": "string"
    },
    "namespace": {
      "type": "string",
      "maxLength": 256,
      "pattern": "^([\\w]|[\\w][\\w@ .-]*[\\w@.-]+)$"
    },
    "package": {
      "type": "string",
      "maxLength": 256,
      "pattern": "^([\\w]|[\\w][\\w@ .-]*[\\w@.-]+)$"
    },
    "action": {
      "type": "string",
      "maxLength": 256,
      "pattern": "^([\\w]|[\\w][\\w@ .-]*[\\w@.-]+)$"
    },
    "result": {
      "type": "boolean",
      "default": true
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 3000
    },
    "keepalive": {
      "type": "boolean",
      "default": true
    },
    "keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 60000
    },
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    }
  },
  "required": ["api_host", "service_token", "namespace", "action"]
}
`

type Config struct {
	APIHost          string `json:"api_host"`
	SSLVerify        *bool  `json:"ssl_verify,omitempty"`
	ServiceToken     string `json:"service_token"`
	Namespace        string `json:"namespace"`
	Package          string `json:"package,omitempty"`
	Action           string `json:"action"`
	Result           *bool  `json:"result,omitempty"`
	Timeout          int    `json:"timeout,omitempty"`
	Keepalive        *bool  `json:"keepalive,omitempty"`
	KeepaliveTimeout int    `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int    `json:"keepalive_pool,omitempty"`
}

type actionResult struct {
	StatusCode int            `json:"statusCode,omitempty"`
	Headers    map[string]any `json:"headers,omitempty"`
	Body       any            `json:"body,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired || !p.serviceTokenSet {
		return secret.ErrCredentialUnavailable
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 5
	}
	if p.config.SSLVerify == nil {
		value := true
		p.config.SSLVerify = &value
	}
	if p.config.Result == nil {
		value := true
		p.config.Result = &value
	}
	if p.config.Keepalive == nil {
		value := true
		p.config.Keepalive = &value
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.config.APIHost)), "http://") {
		logger.Warn("Using openwhisk api_host with no TLS is a security risk")
	}
	if p.client == nil {
		p.client = &http.Client{
			Timeout:   time.Duration(p.config.Timeout) * time.Millisecond,
			Transport: p.transport(),
		}
	}

	return nil
}

func (p *Plugin) transport() *http.Transport {
	transport := httpclient.NewTransport()
	transport.DisableKeepAlives = !*p.config.Keepalive
	transport.IdleConnTimeout = time.Duration(p.config.KeepaliveTimeout) * time.Millisecond
	transport.MaxIdleConnsPerHost = p.config.KeepalivePool
	if !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return transport
}

func (p *Plugin) Config() any {
	return &p.config
}

// MaterializeScopedSecrets resolves the required service token for one
// immutable generation. Public configuration retains only the descriptor of
// the admitted plaintext.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.serviceTokenSet {
		return nil
	}

	value, err := access.Materialize(ctx, "service_token", p.config.ServiceToken)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	if err := value.Use(func(plaintext string) error {
		if strings.TrimSpace(plaintext) == "" {
			return secret.ErrCredentialUnavailable
		}
		return nil
	}); err != nil {
		return secret.ErrCredentialUnavailable
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}

	p.serviceToken = value
	p.serviceTokenSet = true
	p.config.ServiceToken = descriptor.String()
	return nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
// Immutable generation preparation uses MaterializeScopedSecrets instead.
func (p *Plugin) MaterializeSecrets() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.serviceTokenSet {
		return nil
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			base.AdaptRequestPhase(p, next).ServeHTTP(w, r)
			return
		}
		p.serve(w, r)
	})
}

// RunRequestPhase owns the OpenWhisk action response as an upstream result.
// The response source is set before parsing or writing any action response.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
	p.serve(w, r)
	return base.StopRequestWithSource(r, apisixctx.ResponseSourceUpstream)
}

func (p *Plugin) serve(w http.ResponseWriter, r *http.Request) {
	err := p.buildActionRequest(r, func(actionReq *http.Request) error {
		res, err := p.client.Do(actionReq)
		if err != nil {
			logger.Errorf("failed to process openwhisk action, err: %s", err)
			http.Error(w, "failed to process openwhisk action", http.StatusServiceUnavailable)
			return nil
		}
		defer func() { _ = res.Body.Close() }()

		p.writeActionResponse(w, res, r.ProtoMajor >= 2)
		return nil
	})
	if err == nil {
		return
	}
	if errors.Is(err, secret.ErrCredentialUnavailable) {
		http.Error(w, "openwhisk credential unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}

// buildActionRequest keeps request construction, invocation, and cleanup
// inside the private credential callback. The request can therefore be used
// by the transport but never leaves this method with Authorization attached.
func (p *Plugin) buildActionRequest(r *http.Request, use func(*http.Request) error) error {
	if use == nil {
		return secret.ErrCredentialUnavailable
	}
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.retired || p.client == nil {
		return secret.ErrCredentialUnavailable
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	endpoint, err := url.Parse(strings.TrimRight(p.config.APIHost, "/") + p.actionPath())
	if err != nil {
		return fmt.Errorf("invalid api_host: %w", err)
	}
	query := endpoint.Query()
	query.Set("blocking", "true")
	query.Set("result", strconv.FormatBool(*p.config.Result))
	query.Set("timeout", strconv.Itoa(p.config.Timeout))
	endpoint.RawQuery = query.Encode()

	actionReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	actionReq.Header.Set("Content-Type", "application/json")

	return p.useServiceTokenLocked(func(plaintext string) error {
		actionReq.Header.Set(
			"Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(plaintext)),
		)
		defer actionReq.Header.Del("Authorization")
		return use(actionReq)
	})
}

func (p *Plugin) useServiceTokenLocked(use func(string) error) error {
	if p.serviceTokenSet {
		return p.serviceToken.Use(use)
	}
	return secret.ErrCredentialUnavailable
}

// Stop first waits for every request holding the lifecycle read gate, then
// retires the neutral client before releasing the private credential owners.
func (p *Plugin) Stop() {
	if hook := p.testLifecycleHook; hook != nil {
		hook(lifecycleBeforeStopWait)
	}
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return
	}
	p.retired = true
	if p.client != nil {
		p.client.CloseIdleConnections()
		p.client = nil
	}
	p.serviceToken = secret.Value{}
	p.serviceTokenSet = false
}

func (p *Plugin) actionPath() string {
	path := "/api/v1/namespaces/" + url.PathEscape(p.config.Namespace) + "/actions/"
	if p.config.Package != "" {
		path += url.PathEscape(p.config.Package) + "/"
	}
	return path + url.PathEscape(p.config.Action)
}

func (p *Plugin) writeActionResponse(w http.ResponseWriter, res *http.Response, http2 bool) {
	body, err := io.ReadAll(res.Body)
	if err != nil {
		http.Error(w, "failed to read openwhisk response data", http.StatusServiceUnavailable)
		return
	}
	if body == nil {
		w.WriteHeader(res.StatusCode)
		return
	}

	var result actionResult
	if err := json.Unmarshal(body, &result); err != nil {
		http.Error(w, "failed to parse openwhisk response data", http.StatusServiceUnavailable)
		return
	}

	for field, value := range result.Headers {
		setResultHeader(w.Header(), field, value)
	}
	if http2 {
		base.RemoveHTTP2ConnectionHeaders(w.Header())
	}

	status := res.StatusCode
	if result.StatusCode != 0 {
		status = result.StatusCode
	}
	if _, ok := util.TerminalStatus(status); !ok {
		http.Error(w, "failed to parse openwhisk response data", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(resultBody(result.Body, body))
}

func setResultHeader(header http.Header, field string, value any) {
	if values, ok := value.([]any); ok {
		for _, item := range values {
			if encoded, ok := resultHeaderValue(item); ok {
				header.Add(field, encoded)
			}
		}
		return
	}
	if encoded, ok := resultHeaderValue(value); ok {
		header.Set(field, encoded)
	}
}

func resultHeaderValue(value any) (string, bool) {
	switch value.(type) {
	case string, float64, bool:
		return fmt.Sprint(value), true
	default:
		return "", false
	}
}

func resultBody(value any, fallback []byte) []byte {
	switch v := value.(type) {
	case nil:
		return fallback
	case string:
		return []byte(v)
	case bool:
		if !v {
			return fallback
		}
		return []byte("true")
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fallback
		}
		return data
	}
}
