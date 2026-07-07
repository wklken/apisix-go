package ai_proxy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
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
	priority = 1040
	name     = "ai-proxy"
)

const schema = `
{
  "type": "object",
  "properties": {
    "provider": {
      "type": "string",
      "enum": [
        "openai",
        "deepseek",
        "azure-openai",
        "aimlapi",
        "anthropic",
        "openrouter",
        "gemini",
        "vertex-ai",
        "bedrock",
        "openai-compatible"
      ]
    },
    "provider_conf": {
      "type": "object",
      "properties": {
        "project_id": {
          "type": "string"
        },
        "region": {
          "type": "string",
          "minLength": 1
        }
      }
    },
    "auth": {
      "type": "object",
      "properties": {
        "header": {
          "$ref": "#/$defs/auth_items"
        },
        "query": {
          "$ref": "#/$defs/auth_items"
        },
        "gcp": {
          "type": "object"
        },
        "aws": {
          "type": "object",
          "properties": {
            "access_key_id": {
              "type": "string",
              "minLength": 1
            },
            "secret_access_key": {
              "type": "string",
              "minLength": 1
            },
            "session_token": {
              "type": "string",
              "minLength": 1
            }
          },
          "required": ["access_key_id", "secret_access_key"]
        }
      },
      "additionalProperties": false
    },
    "options": {
      "type": "object",
      "properties": {
        "model": {
          "type": "string"
        }
      },
      "additionalProperties": true
    },
    "override": {
      "type": "object",
      "properties": {
        "endpoint": {
          "type": "string",
          "minLength": 1
        },
        "llm_options": {
          "type": "object",
          "properties": {
            "max_tokens": {
              "type": "integer",
              "minimum": 1
            }
          },
          "additionalProperties": false
        },
        "request_body": {
          "type": "object",
          "additionalProperties": true
        },
        "request_body_force_override": {
          "type": "boolean",
          "default": false
        }
      }
    },
    "logging": {
      "type": "object",
      "properties": {
        "summaries": {
          "type": "boolean",
          "default": false
        },
        "payloads": {
          "type": "boolean",
          "default": false
        }
      }
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 600000,
      "default": 30000
    },
    "max_req_body_size": {
      "type": "integer",
      "minimum": 1,
      "default": 67108864
    },
    "max_response_bytes": {
      "type": "integer",
      "minimum": 1
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
      "default": 30
    },
    "streaming_flush_interval_ms": {
      "type": "integer",
      "minimum": 0,
      "default": 10
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    }
  },
  "required": ["provider", "auth"],
  "$defs": {
    "auth_items": {
      "type": "object",
      "patternProperties": {
        "^[a-zA-Z0-9._-]+$": {
          "type": "string"
        }
      }
    }
  }
}
`

type Config struct {
	Provider                 string         `json:"provider"`
	ProviderConf             map[string]any `json:"provider_conf,omitempty"`
	Auth                     Auth           `json:"auth"`
	Options                  map[string]any `json:"options,omitempty"`
	Override                 Override       `json:"override,omitempty"`
	Logging                  Logging        `json:"logging,omitempty"`
	Timeout                  int            `json:"timeout,omitempty"`
	MaxReqBodySize           int64          `json:"max_req_body_size,omitempty"`
	MaxResponseBytes         int64          `json:"max_response_bytes,omitempty"`
	Keepalive                *bool          `json:"keepalive,omitempty"`
	KeepaliveTimeout         int            `json:"keepalive_timeout,omitempty"`
	KeepalivePool            int            `json:"keepalive_pool,omitempty"`
	StreamingFlushIntervalMS int            `json:"streaming_flush_interval_ms,omitempty"`
	SSLVerify                *bool          `json:"ssl_verify,omitempty"`
}

type Auth struct {
	Header map[string]string `json:"header,omitempty"`
	Query  map[string]string `json:"query,omitempty"`
}

type Override struct {
	Endpoint                 string         `json:"endpoint,omitempty"`
	LLMOptions               LLMOptions     `json:"llm_options,omitempty"`
	RequestBody              map[string]any `json:"request_body,omitempty"`
	RequestBodyForceOverride *bool          `json:"request_body_force_override,omitempty"`
}

type LLMOptions struct {
	MaxTokens int `json:"max_tokens,omitempty"`
}

type Logging struct {
	Summaries bool `json:"summaries,omitempty"`
	Payloads  bool `json:"payloads,omitempty"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if (p.config.Provider == "openai-compatible" || p.config.Provider == "azure-openai") &&
		p.config.Override.Endpoint == "" {
		return fmt.Errorf("override.endpoint is required for %s provider", p.config.Provider)
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 30000
	}
	if p.config.MaxReqBodySize == 0 {
		p.config.MaxReqBodySize = 64 * 1024 * 1024
	}
	if p.config.Keepalive == nil {
		keepalive := true
		p.config.Keepalive = &keepalive
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 30
	}
	if p.config.StreamingFlushIntervalMS == 0 {
		p.config.StreamingFlushIntervalMS = 10
	}
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}

	p.client = &http.Client{
		Timeout:   time.Duration(p.config.Timeout) * time.Millisecond,
		Transport: p.transport(),
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		body, err := p.readJSONBody(r)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "max_req_body_size") {
				status = http.StatusRequestEntityTooLarge
			}
			writeJSONMessage(w, status, err.Error())
			return
		}

		proxyReq, err := p.buildProviderRequest(r, body)
		if err != nil {
			writeJSONMessage(w, http.StatusBadGateway, err.Error())
			return
		}
		resp, err := p.client.Do(proxyReq)
		if err != nil {
			writeJSONMessage(w, http.StatusServiceUnavailable, "failed to request LLM: "+err.Error())
			return
		}
		defer resp.Body.Close()

		p.writeProviderResponse(w, resp)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) readJSONBody(r *http.Request) ([]byte, error) {
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return nil, fmt.Errorf("unsupported content-type: %s, only application/json is supported", contentType)
	}
	if r.ContentLength > p.config.MaxReqBodySize {
		return nil, fmt.Errorf("request body exceeds max_req_body_size")
	}

	reader := io.LimitReader(r.Body, p.config.MaxReqBodySize+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("could not get body: %w", err)
	}
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("could not get body: %w", err)
	}
	if int64(len(body)) > p.config.MaxReqBodySize {
		return nil, fmt.Errorf("request body exceeds max_req_body_size")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("missing request body")
	}

	var bodyTab map[string]any
	if err := json.Unmarshal(body, &bodyTab); err != nil {
		return nil, fmt.Errorf("could not parse JSON request body: %w", err)
	}
	for key, value := range p.config.Options {
		bodyTab[key] = value
	}
	p.applyLLMOptions(bodyTab)

	rewritten, err := json.Marshal(bodyTab)
	if err != nil {
		return nil, fmt.Errorf("failed to encode provider request body: %w", err)
	}
	return rewritten, nil
}

func (p *Plugin) applyLLMOptions(body map[string]any) {
	if p.config.Override.LLMOptions.MaxTokens == 0 {
		return
	}
	switch p.config.Provider {
	case "openai":
		body["max_completion_tokens"] = p.config.Override.LLMOptions.MaxTokens
		delete(body, "max_tokens")
	default:
		body["max_tokens"] = p.config.Override.LLMOptions.MaxTokens
	}
}

func (p *Plugin) buildProviderRequest(r *http.Request, body []byte) (*http.Request, error) {
	endpoint, err := p.endpoint()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM request: %w", err)
	}
	copyForwardHeaders(req.Header, r.Header)
	req.Header.Set("Content-Type", "application/json")
	for header, value := range p.config.Auth.Header {
		req.Header.Set(header, value)
	}
	query := req.URL.Query()
	for key, value := range p.config.Auth.Query {
		query.Set(key, value)
	}
	req.URL.RawQuery = query.Encode()

	return req, nil
}

func copyForwardHeaders(dst, src http.Header) {
	for field, values := range src {
		switch strings.ToLower(field) {
		case "host", "content-length", "accept-encoding":
			continue
		}
		for _, value := range values {
			dst.Add(field, value)
		}
	}
}

func (p *Plugin) endpoint() (string, error) {
	if p.config.Override.Endpoint != "" {
		return p.config.Override.Endpoint, nil
	}

	switch p.config.Provider {
	case "openai":
		return "https://api.openai.com/v1/chat/completions", nil
	case "deepseek":
		return "https://api.deepseek.com/chat/completions", nil
	case "aimlapi":
		return "https://api.aimlapi.com/v1/chat/completions", nil
	case "openrouter":
		return "https://openrouter.ai/api/v1/chat/completions", nil
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", nil
	default:
		return "", fmt.Errorf("provider %q requires override.endpoint in apisix-go", p.config.Provider)
	}
}

func (p *Plugin) writeProviderResponse(w http.ResponseWriter, resp *http.Response) {
	bodyReader := io.Reader(resp.Body)
	if p.config.MaxResponseBytes > 0 {
		bodyReader = io.LimitReader(resp.Body, p.config.MaxResponseBytes+1)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		writeJSONMessage(w, http.StatusBadGateway, "failed to read LLM response body: "+err.Error())
		return
	}
	if p.config.MaxResponseBytes > 0 && int64(len(body)) > p.config.MaxResponseBytes {
		writeJSONMessage(w, http.StatusBadGateway, "max_response_bytes exceeded")
		return
	}

	for field, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = p.config.KeepalivePool
	transport.IdleConnTimeout = time.Duration(p.config.KeepaliveTimeout) * time.Millisecond
	if p.config.Keepalive != nil && !*p.config.Keepalive {
		transport.DisableKeepAlives = true
	}
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return transport
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
