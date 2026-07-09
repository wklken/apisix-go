package ai_request_rewrite

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
	priority = 1073
	name     = "ai-request-rewrite"
)

const schema = `
{
  "type": "object",
  "properties": {
    "prompt": {
      "type": "string"
    },
    "provider": {
      "type": "string",
      "enum": [
        "openai",
        "deepseek",
        "aimlapi",
        "anthropic",
        "openai-compatible",
        "azure-openai",
        "openrouter",
        "gemini",
        "vertex-ai",
        "bedrock"
      ]
    },
    "auth": {
      "type": "object",
      "properties": {
        "header": {
          "$ref": "#/$defs/auth_items"
        },
        "query": {
          "$ref": "#/$defs/auth_items"
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
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 30000
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
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "override": {
      "type": "object",
      "properties": {
        "endpoint": {
          "type": "string"
        }
      }
    }
  },
  "required": ["prompt", "provider", "auth"],
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
	Prompt           string         `json:"prompt"`
	Provider         string         `json:"provider"`
	Auth             Auth           `json:"auth"`
	Options          map[string]any `json:"options,omitempty"`
	Timeout          int            `json:"timeout,omitempty"`
	Keepalive        *bool          `json:"keepalive,omitempty"`
	KeepaliveTimeout int            `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int            `json:"keepalive_pool,omitempty"`
	SSLVerify        *bool          `json:"ssl_verify,omitempty"`
	Override         Override       `json:"override,omitempty"`
}

type Auth struct {
	Header map[string]string `json:"header,omitempty"`
	Query  map[string]string `json:"query,omitempty"`
}

type Override struct {
	Endpoint string `json:"endpoint,omitempty"`
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
		body, err := readBody(r)
		if err != nil {
			writeJSONMessage(w, http.StatusBadRequest, "could not get body: "+err.Error())
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			writeJSONMessage(w, http.StatusBadRequest, "missing request body")
			return
		}

		llmResp, err := p.requestLLM(r, string(body))
		if err != nil {
			writeJSONMessage(w, http.StatusInternalServerError, err.Error())
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(llmResp))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(llmResp)), nil
		}
		r.ContentLength = int64(len(llmResp))
		r.Header.Set("Content-Length", fmt.Sprint(len(llmResp)))

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) requestLLM(r *http.Request, originalBody string) ([]byte, error) {
	endpoint, err := p.endpoint()
	if err != nil {
		return nil, err
	}

	llmRequest := buildOpenAIChatRequest(p.config.Prompt, originalBody, p.config.Options)
	p.applyProviderBodyRules(llmRequest)

	llmBody, err := json.Marshal(llmRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to encode LLM request body: %w", err)
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(llmBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for header, value := range p.config.Auth.Header {
		req.Header.Set(header, value)
	}
	query := req.URL.Query()
	for key, value := range p.config.Auth.Query {
		query.Set(key, value)
	}
	req.URL.RawQuery = query.Encode()

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to request LLM: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read LLM response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM service returned error status: %d", resp.StatusCode)
	}

	rewritten, err := extractOpenAIChatContent(rawBody)
	if err != nil {
		return nil, err
	}
	return []byte(rewritten), nil
}

func (p *Plugin) applyProviderBodyRules(body map[string]any) {
	if p.config.Provider == "azure-openai" {
		delete(body, "model")
	}
}

func (p *Plugin) endpoint() (string, error) {
	if p.config.Override.Endpoint != "" {
		return p.config.Override.Endpoint, nil
	}

	switch p.config.Provider {
	case "openai":
		return "https://api.openai.com/v1/chat/completions", nil
	case "aimlapi":
		return "https://api.aimlapi.com/v1/chat/completions", nil
	case "openrouter":
		return "https://openrouter.ai/api/v1/chat/completions", nil
	default:
		return "", fmt.Errorf("provider %q requires override.endpoint in apisix-go", p.config.Provider)
	}
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

func buildOpenAIChatRequest(prompt string, originalBody string, options map[string]any) map[string]any {
	body := map[string]any{
		"messages": []map[string]string{
			{"role": "system", "content": prompt},
			{"role": "user", "content": originalBody},
		},
		"stream": false,
	}
	for key, value := range options {
		body[key] = value
	}
	return body
}

func extractOpenAIChatContent(rawBody []byte) (string, error) {
	var decoded map[string]any
	if err := json.Unmarshal(rawBody, &decoded); err != nil {
		return "", fmt.Errorf("failed to decode LLM response: %w", err)
	}

	choices, ok := decoded["choices"].([]any)
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("failed to extract text from LLM response")
	}

	parts := make([]string, 0, len(choices))
	for _, choiceValue := range choices {
		choice, ok := choiceValue.(map[string]any)
		if !ok {
			continue
		}
		message, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].(string)
		if ok {
			parts = append(parts, content)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("failed to extract text from LLM response")
	}
	return strings.Join(parts, " "), nil
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
