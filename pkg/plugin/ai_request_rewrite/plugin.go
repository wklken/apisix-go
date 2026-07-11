package ai_request_rewrite

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config    Config
	client    *http.Client
	now       func() time.Time
	gcpTokens gcpTokenApplier
}

type gcpTokenApplier interface {
	Apply(context.Context, *http.Client, *http.Request, ai_auth.GCPConfig) error
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
        },
		"aws": {
		  "type": "object",
		  "properties": {
		    "access_key_id": {"type": "string", "minLength": 1},
		    "secret_access_key": {"type": "string", "minLength": 1},
		    "session_token": {"type": "string", "minLength": 1}
		  },
		  "required": ["access_key_id", "secret_access_key"]
		},
		"gcp": {
		  "type": "object",
		  "properties": {
		    "service_account_json": {"type": "string"},
		    "max_ttl": {"type": "integer", "minimum": 1},
		    "expire_early_secs": {"type": "integer", "minimum": 0}
		  }
		}
      },
      "additionalProperties": false
    },
    "provider_conf": {
      "type": "object",
      "properties": {
        "project_id": {"type": "string"},
        "region": {"type": "string", "minLength": 1}
      }
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
	ProviderConf     map[string]any `json:"provider_conf,omitempty"`
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
	Header map[string]string  `json:"header,omitempty"`
	Query  map[string]string  `json:"query,omitempty"`
	AWS    *ai_auth.AWSConfig `json:"aws,omitempty"`
	GCP    *ai_auth.GCPConfig `json:"gcp,omitempty"`
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
	if p.config.Provider == "bedrock" {
		if region, _ := p.config.ProviderConf["region"].(string); region == "" {
			return fmt.Errorf("bedrock requires provider_conf.region")
		}
		if p.config.Auth.AWS == nil {
			return fmt.Errorf("bedrock requires auth.aws")
		}
	}
	if p.config.Provider == "vertex-ai" && p.config.Override.Endpoint == "" {
		projectID, _ := p.config.ProviderConf["project_id"].(string)
		region, _ := p.config.ProviderConf["region"].(string)
		if projectID == "" || region == "" {
			return fmt.Errorf("vertex-ai requires provider_conf project_id and region or override.endpoint")
		}
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
	if p.now == nil {
		p.now = time.Now
	}
	if p.gcpTokens == nil {
		p.gcpTokens = ai_auth.NewGCPTokenSource()
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
	protocol := preferredProtocol(p.config.Provider)
	endpoint, err := p.endpoint(protocol)
	if err != nil {
		return nil, err
	}

	llmRequest := ai_protocols.BuildSimpleRequest(protocol, p.config.Prompt, originalBody, p.config.Options)
	p.applyProviderBodyRules(llmRequest)
	registerLLMRewriteRequestVars(r, llmRequest)

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
	if p.config.Auth.GCP != nil {
		if err := p.gcpTokens.Apply(r.Context(), p.client, req, *p.config.Auth.GCP); err != nil {
			return nil, fmt.Errorf("authenticate GCP request: %w", err)
		}
	}
	if p.config.Provider == "bedrock" {
		region, _ := p.config.ProviderConf["region"].(string)
		if err := ai_auth.SignAWSRequest(req, llmBody, *p.config.Auth.AWS, region, "bedrock", p.now()); err != nil {
			return nil, fmt.Errorf("sign Bedrock request: %w", err)
		}
	}

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

	var responseBody map[string]any
	if err := json.Unmarshal(rawBody, &responseBody); err != nil {
		return nil, fmt.Errorf("failed to decode LLM response: %w", err)
	}
	rewritten := ai_protocols.ExtractResponseText(protocol, responseBody)
	if rewritten == "" {
		return nil, fmt.Errorf("failed to extract text from LLM response")
	}
	return []byte(rewritten), nil
}

func registerLLMRewriteRequestVars(r *http.Request, body map[string]any) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}

	apisixctx.RegisterRequestVar(r, "$llm_request_body", body)
	apisixctx.RegisterRequestVar(r, "$llm_request_start_time", float64(time.Now().UnixNano())/float64(time.Second))
	apisixctx.RegisterRequestVar(r, "$ai_request_body_changed", true)
}

func (p *Plugin) applyProviderBodyRules(body map[string]any) {
	if p.config.Provider == "azure-openai" {
		delete(body, "model")
	}
}

func (p *Plugin) endpoint(protocol ai_protocols.Protocol) (string, error) {
	if p.config.Override.Endpoint != "" {
		return appendProviderPath(p.config.Override.Endpoint, p.providerPath(protocol))
	}

	switch p.config.Provider {
	case "openai":
		return "https://api.openai.com/v1/chat/completions", nil
	case "deepseek":
		return "https://api.deepseek.com/chat/completions", nil
	case "aimlapi":
		return "https://api.aimlapi.com/chat/completions", nil
	case "anthropic":
		return "https://api.anthropic.com" + protocol.Endpoint, nil
	case "openrouter":
		return "https://openrouter.ai/api/v1/chat/completions", nil
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", nil
	case "vertex-ai", "bedrock":
		path := p.providerPath(protocol)
		if path == "" {
			return "", fmt.Errorf("provider %q requires provider_conf and options.model", p.config.Provider)
		}
		return p.providerBaseURL() + path, nil
	default:
		return "", fmt.Errorf("provider %q requires override.endpoint in apisix-go", p.config.Provider)
	}
}

func preferredProtocol(provider string) ai_protocols.Protocol {
	if provider == "bedrock" {
		return ai_protocols.BedrockConverse
	}
	return ai_protocols.OpenAIChat
}

func (p *Plugin) providerBaseURL() string {
	region, _ := p.config.ProviderConf["region"].(string)
	switch p.config.Provider {
	case "vertex-ai":
		return "https://" + region + "-aiplatform.googleapis.com"
	case "bedrock":
		return "https://bedrock-runtime." + region + ".amazonaws.com"
	default:
		return ""
	}
}

func (p *Plugin) providerPath(protocol ai_protocols.Protocol) string {
	model, _ := p.config.Options["model"].(string)
	region, _ := p.config.ProviderConf["region"].(string)
	projectID, _ := p.config.ProviderConf["project_id"].(string)
	switch p.config.Provider {
	case "vertex-ai":
		if projectID == "" || region == "" {
			return ""
		}
		return fmt.Sprintf(
			"/v1beta1/projects/%s/locations/%s/endpoints/openapi/chat/completions",
			url.PathEscape(projectID),
			url.PathEscape(region),
		)
	case "bedrock":
		if region == "" || model == "" {
			return ""
		}
		return "/model/" + url.PathEscape(model) + "/converse"
	default:
		return protocol.Endpoint
	}
}

func appendProviderPath(endpoint string, providerPath string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse provider endpoint: %w", err)
	}
	if (parsed.Path == "" || parsed.Path == "/") && providerPath != "" {
		parsed.Path = providerPath
	}
	return parsed.String(), nil
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
