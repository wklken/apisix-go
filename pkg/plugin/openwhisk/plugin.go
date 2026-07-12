package openwhisk

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client
}

const (
	priority = -1901
	name     = "openwhisk"
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
	if p.client == nil {
		p.client = &http.Client{
			Timeout:   time.Duration(p.config.Timeout) * time.Millisecond,
			Transport: p.transport(),
		}
	}

	return nil
}

func (p *Plugin) transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
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

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actionReq, err := p.buildActionRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		res, err := p.client.Do(actionReq)
		if err != nil {
			http.Error(w, "failed to process openwhisk action", http.StatusServiceUnavailable)
			return
		}
		defer res.Body.Close()

		p.writeActionResponse(w, res)
	})
}

func (p *Plugin) buildActionRequest(r *http.Request) (*http.Request, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	endpoint, err := url.Parse(strings.TrimRight(p.config.APIHost, "/") + p.actionPath())
	if err != nil {
		return nil, fmt.Errorf("invalid api_host: %w", err)
	}
	query := endpoint.Query()
	query.Set("blocking", "true")
	query.Set("result", strconv.FormatBool(*p.config.Result))
	query.Set("timeout", strconv.Itoa(p.config.Timeout))
	endpoint.RawQuery = query.Encode()

	actionReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	actionReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.config.ServiceToken)))
	actionReq.Header.Set("Content-Type", "application/json")
	return actionReq, nil
}

func (p *Plugin) actionPath() string {
	path := "/api/v1/namespaces/" + url.PathEscape(p.config.Namespace) + "/actions/"
	if p.config.Package != "" {
		path += url.PathEscape(p.config.Package) + "/"
	}
	return path + url.PathEscape(p.config.Action)
}

func (p *Plugin) writeActionResponse(w http.ResponseWriter, res *http.Response) {
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

	status := res.StatusCode
	if result.StatusCode != 0 {
		status = result.StatusCode
	}
	w.WriteHeader(status)
	w.Write(resultBody(result.Body, body))
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
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fallback
		}
		return data
	}
}
