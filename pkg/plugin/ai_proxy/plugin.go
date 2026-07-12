package ai_proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
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

type preparedProviderRequest struct {
	clientBody          []byte
	providerBody        []byte
	clientProtocol      ai_protocols.Protocol
	providerProtocol    ai_protocols.Protocol
	toolNameMap         map[string]string
	anthropicConversion bool
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
	"max_stream_duration_ms": {
	  "type": "integer",
	  "minimum": 1
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
	Override                 Override       `json:"override"`
	Logging                  Logging        `json:"logging"`
	Timeout                  int            `json:"timeout,omitempty"`
	MaxReqBodySize           int64          `json:"max_req_body_size,omitempty"`
	MaxStreamDurationMS      int            `json:"max_stream_duration_ms,omitempty"`
	MaxResponseBytes         int64          `json:"max_response_bytes,omitempty"`
	Keepalive                *bool          `json:"keepalive,omitempty"`
	KeepaliveTimeout         int            `json:"keepalive_timeout,omitempty"`
	KeepalivePool            int            `json:"keepalive_pool,omitempty"`
	StreamingFlushIntervalMS *int           `json:"streaming_flush_interval_ms,omitempty"`
	SSLVerify                *bool          `json:"ssl_verify,omitempty"`
}

type Auth struct {
	Header map[string]string  `json:"header,omitempty"`
	Query  map[string]string  `json:"query,omitempty"`
	AWS    *ai_auth.AWSConfig `json:"aws,omitempty"`
	GCP    *ai_auth.GCPConfig `json:"gcp,omitempty"`
}

type Override struct {
	Endpoint                 string         `json:"endpoint,omitempty"`
	LLMOptions               LLMOptions     `json:"llm_options"`
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

func (p *Plugin) Config() any {
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
	if p.config.StreamingFlushIntervalMS == nil {
		flushInterval := 10
		p.config.StreamingFlushIntervalMS = &flushInterval
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
		body, protocol, err := p.readJSONBody(r)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "max_req_body_size") {
				status = http.StatusRequestEntityTooLarge
			}
			writeJSONMessage(w, status, err.Error())
			return
		}
		r = ai_runtime.WithExecution(r, "ai-proxy-"+p.config.Provider, func(
			w http.ResponseWriter,
			r *http.Request,
		) {
			p.executeProviderRequest(w, r, body, protocol)
		})
		ai_runtime.FromRequest(r).SetStreaming(requestIsStreaming(body, protocol))
		if ai_runtime.TerminalEnabled(r) {
			next.ServeHTTP(w, r)
			return
		}
		ai_runtime.FromRequest(r).Execute(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) executeProviderRequest(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	protocol ai_protocols.Protocol,
) {
	p.registerRequestIdentity(r, body, protocol)
	started := ai_runtime.StartLLMRequest(r)
	defer func() {
		ai_runtime.MarkLLMRequestDone(r, started)
		ai_runtime.RegisterLogging(r, p.config.Logging.Summaries, p.config.Logging.Payloads, protocol, body)
	}()
	doneMetric := metrics.BeginLLMRequest(r)
	defer doneMetric()
	prepared, err := p.prepareProviderRequest(body, protocol)
	if err != nil {
		writeJSONMessage(w, http.StatusBadGateway, err.Error())
		return
	}
	proxyReq, err := p.buildProviderRequest(r, prepared.providerBody, prepared.providerProtocol)
	if err != nil {
		writeJSONMessage(w, http.StatusBadGateway, err.Error())
		return
	}
	if prepared.anthropicConversion {
		ai_protocols.ConvertAnthropicHeadersToOpenAI(proxyReq.Header)
	}
	if requestIsStreaming(prepared.clientBody, prepared.clientProtocol) && p.config.MaxStreamDurationMS > 0 {
		deadlineContext, cancel := context.WithTimeout(
			proxyReq.Context(),
			time.Duration(p.config.MaxStreamDurationMS)*time.Millisecond,
		)
		defer cancel()
		proxyReq = proxyReq.WithContext(deadlineContext)
	}
	resp, err := p.client.Do(proxyReq)
	if err != nil {
		writeJSONMessage(w, http.StatusServiceUnavailable, "failed to request LLM: "+err.Error())
		return
	}
	defer resp.Body.Close()

	p.writeProviderResponse(w, r, prepared, started, resp)
}

func (p *Plugin) registerRequestIdentity(r *http.Request, body []byte, protocol ai_protocols.Protocol) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	requestType := protocol.RequestType
	if requestIsStreaming(body, protocol) {
		requestType = "ai_stream"
	}
	apisixctx.RegisterRequestVar(r, "$request_type", requestType)
	var decoded map[string]any
	if json.Unmarshal(body, &decoded) == nil {
		apisixctx.RegisterRequestVar(r, "$llm_request_body", decoded)
	}
	if model := p.requestModel(body); model != "" {
		apisixctx.RegisterRequestVar(r, "$request_llm_model", model)
		apisixctx.RegisterRequestVar(r, "$llm_model", model)
	}
}

func (p *Plugin) prepareProviderRequest(
	body []byte,
	protocol ai_protocols.Protocol,
) (preparedProviderRequest, error) {
	prepared := preparedProviderRequest{
		clientBody: body, providerBody: body, clientProtocol: protocol, providerProtocol: protocol,
	}
	if protocol != ai_protocols.AnthropicMessages || !providerUsesOpenAIChat(p.config.Provider) {
		return prepared, nil
	}
	converted, toolNameMap, err := ai_protocols.ConvertAnthropicMessagesToOpenAI(body)
	if err != nil {
		return prepared, fmt.Errorf("convert Anthropic request to OpenAI Chat: %w", err)
	}
	var convertedBody map[string]any
	if err := json.Unmarshal(converted, &convertedBody); err != nil {
		return prepared, fmt.Errorf("decode converted OpenAI Chat request: %w", err)
	}
	p.applyLLMOptions(convertedBody, ai_protocols.OpenAIChat)
	p.applyRequestBodyOverride(convertedBody, ai_protocols.OpenAIChat)
	p.applyProviderBodyRules(convertedBody)
	if ai_protocols.IsStreaming(ai_protocols.OpenAIChat, convertedBody) {
		convertedBody["stream_options"] = map[string]any{"include_usage": true}
	}
	converted, err = json.Marshal(convertedBody)
	if err != nil {
		return prepared, fmt.Errorf("encode converted OpenAI Chat request: %w", err)
	}
	prepared.providerBody = converted
	prepared.providerProtocol = ai_protocols.OpenAIChat
	prepared.toolNameMap = toolNameMap
	prepared.anthropicConversion = true
	return prepared, nil
}

func providerUsesOpenAIChat(provider string) bool {
	switch provider {
	case "openai", "deepseek", "aimlapi", "openai-compatible", "azure-openai", "openrouter", "gemini",
		"vertex-ai":
		return true
	default:
		return false
	}
}

func (p *Plugin) readJSONBody(r *http.Request) ([]byte, ai_protocols.Protocol, error) {
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return nil, ai_protocols.Protocol{}, fmt.Errorf(
			"unsupported content-type: %s, only application/json is supported",
			contentType,
		)
	}
	if r.ContentLength > p.config.MaxReqBodySize {
		return nil, ai_protocols.Protocol{}, fmt.Errorf("request body exceeds max_req_body_size")
	}

	reader := io.LimitReader(r.Body, p.config.MaxReqBodySize+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, ai_protocols.Protocol{}, fmt.Errorf("could not get body: %w", err)
	}
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil, ai_protocols.Protocol{}, fmt.Errorf("could not get body: %w", err)
	}
	if int64(len(body)) > p.config.MaxReqBodySize {
		return nil, ai_protocols.Protocol{}, fmt.Errorf("request body exceeds max_req_body_size")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, ai_protocols.Protocol{}, fmt.Errorf("missing request body")
	}

	var bodyTab map[string]any
	if err := json.Unmarshal(body, &bodyTab); err != nil {
		return nil, ai_protocols.Protocol{}, fmt.Errorf("could not parse JSON request body: %w", err)
	}
	protocol, err := ai_protocols.Detect(r.URL.Path, bodyTab)
	if err != nil {
		return nil, ai_protocols.Protocol{}, err
	}
	maps.Copy(bodyTab, p.config.Options)
	if protocol != ai_protocols.AnthropicMessages || !providerUsesOpenAIChat(p.config.Provider) {
		p.applyLLMOptions(bodyTab, protocol)
		p.applyRequestBodyOverride(bodyTab, protocol)
		p.applyProviderBodyRules(bodyTab)
		if ai_protocols.IsStreaming(protocol, bodyTab) && protocol == ai_protocols.OpenAIChat {
			bodyTab["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	rewritten, err := json.Marshal(bodyTab)
	if err != nil {
		return nil, ai_protocols.Protocol{}, fmt.Errorf("failed to encode provider request body: %w", err)
	}
	return rewritten, protocol, nil
}

func (p *Plugin) applyRequestBodyOverride(body map[string]any, protocol ai_protocols.Protocol) {
	override := p.requestBodyOverride(protocol)
	if len(override) == 0 {
		return
	}
	force := p.config.Override.RequestBodyForceOverride != nil && *p.config.Override.RequestBodyForceOverride
	mergeBodyMap(body, override, force)
}

func (p *Plugin) requestBodyOverride(protocol ai_protocols.Protocol) map[string]any {
	if len(p.config.Override.RequestBody) == 0 {
		return nil
	}
	if override, ok := asAnyMap(p.config.Override.RequestBody[protocol.OverrideKey]); ok {
		return override
	}
	if hasProtocolRequestBodyOverride(p.config.Override.RequestBody) {
		return nil
	}
	if protocol != ai_protocols.OpenAIChat {
		return nil
	}
	return p.config.Override.RequestBody
}

func hasProtocolRequestBodyOverride(values map[string]any) bool {
	for key := range values {
		switch key {
		case "openai-chat", "openai-responses", "openai-embeddings", "anthropic-messages",
			"bedrock-converse", "passthrough":
			return true
		}
	}
	return false
}

func mergeBodyMap(dst map[string]any, override map[string]any, force bool) {
	for key, overrideValue := range override {
		currentValue, exists := dst[key]
		currentMap, currentIsMap := asAnyMap(currentValue)
		overrideMap, overrideIsMap := asAnyMap(overrideValue)
		if exists && currentIsMap && overrideIsMap {
			mergeBodyMap(currentMap, overrideMap, force)
			continue
		}
		if !exists || force {
			dst[key] = cloneJSONValue(overrideValue)
		}
	}
}

func asAnyMap(value any) (map[string]any, bool) {
	out, ok := value.(map[string]any)
	return out, ok
}

func cloneJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneJSONValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneJSONValue(item)
		}
		return out
	default:
		return value
	}
}

func (p *Plugin) applyProviderBodyRules(body map[string]any) {
	if p.config.Provider == "azure-openai" {
		delete(body, "model")
	}
}

func (p *Plugin) applyLLMOptions(body map[string]any, protocol ai_protocols.Protocol) {
	if p.config.Override.LLMOptions.MaxTokens == 0 {
		return
	}
	if protocol == ai_protocols.OpenAIEmbeddings {
		return
	}
	switch p.config.Provider {
	case "openai":
		switch protocol {
		case ai_protocols.OpenAIChat:
			body["max_completion_tokens"] = p.config.Override.LLMOptions.MaxTokens
			delete(body, "max_tokens")
		case ai_protocols.OpenAIResponses:
			body["max_output_tokens"] = p.config.Override.LLMOptions.MaxTokens
		}
	case "openai-compatible":
		switch protocol {
		case ai_protocols.OpenAIChat:
			body["max_tokens"] = p.config.Override.LLMOptions.MaxTokens
		case ai_protocols.OpenAIResponses:
			body["max_output_tokens"] = p.config.Override.LLMOptions.MaxTokens
		}
	case "gemini", "vertex-ai":
		if protocol == ai_protocols.OpenAIChat {
			body["max_completion_tokens"] = p.config.Override.LLMOptions.MaxTokens
		}
	case "bedrock":
		if protocol == ai_protocols.BedrockConverse {
			inferenceConfig, _ := body["inferenceConfig"].(map[string]any)
			if inferenceConfig == nil {
				inferenceConfig = make(map[string]any)
				body["inferenceConfig"] = inferenceConfig
			}
			inferenceConfig["maxTokens"] = p.config.Override.LLMOptions.MaxTokens
		}
	default:
		if protocol == ai_protocols.OpenAIChat {
			body["max_tokens"] = p.config.Override.LLMOptions.MaxTokens
		}
	}
}

func (p *Plugin) buildProviderRequest(
	r *http.Request,
	body []byte,
	protocol ai_protocols.Protocol,
) (*http.Request, error) {
	endpoint, err := p.endpoint(protocol, body)
	if err != nil {
		return nil, err
	}
	providerBody, err := p.finalProviderBody(body, protocol)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(providerBody))
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
	if p.config.Auth.GCP != nil {
		if err := p.gcpTokens.Apply(r.Context(), p.client, req, *p.config.Auth.GCP); err != nil {
			return nil, fmt.Errorf("authenticate GCP request: %w", err)
		}
	}
	if p.config.Provider == "bedrock" {
		region, _ := p.config.ProviderConf["region"].(string)
		if err := ai_auth.SignAWSRequest(
			req,
			providerBody,
			*p.config.Auth.AWS,
			region,
			"bedrock",
			p.now(),
		); err != nil {
			return nil, fmt.Errorf("sign Bedrock request: %w", err)
		}
	}

	return req, nil
}

func (p *Plugin) finalProviderBody(body []byte, protocol ai_protocols.Protocol) ([]byte, error) {
	if p.config.Provider == "vertex-ai" && protocol == ai_protocols.OpenAIEmbeddings {
		return ai_protocols.ConvertOpenAIEmbeddingsToVertex(body)
	}
	if p.config.Provider != "bedrock" || protocol != ai_protocols.BedrockConverse {
		return body, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode Bedrock request body: %w", err)
	}
	delete(decoded, "model")
	delete(decoded, "stream")
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("encode Bedrock request body: %w", err)
	}
	return encoded, nil
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

func (p *Plugin) endpoint(protocol ai_protocols.Protocol, body []byte) (string, error) {
	if p.config.Override.Endpoint != "" {
		if p.config.Provider == "openai-compatible" {
			return appendProtocolEndpoint(p.config.Override.Endpoint, protocol)
		}
		if p.config.Provider == "bedrock" {
			return appendBedrockEndpoint(
				p.config.Override.Endpoint,
				p.requestModel(body),
				requestIsStreaming(body, protocol),
			)
		}
		return p.config.Override.Endpoint, nil
	}

	switch p.config.Provider {
	case "openai":
		return "https://api.openai.com" + protocol.Endpoint, nil
	case "deepseek":
		return "https://api.deepseek.com/chat/completions", nil
	case "aimlapi":
		return "https://api.aimlapi.com/v1/chat/completions", nil
	case "openrouter":
		return "https://openrouter.ai/api/v1/chat/completions", nil
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", nil
	case "anthropic":
		return "https://api.anthropic.com" + protocol.Endpoint, nil
	case "bedrock":
		region, _ := p.config.ProviderConf["region"].(string)
		return appendBedrockEndpoint(
			"https://bedrock-runtime."+region+".amazonaws.com",
			p.requestModel(body),
			requestIsStreaming(body, protocol),
		)
	case "vertex-ai":
		return p.vertexEndpoint(protocol, body)
	default:
		return "", fmt.Errorf("provider %q requires override.endpoint in apisix-go", p.config.Provider)
	}
}

func (p *Plugin) vertexEndpoint(protocol ai_protocols.Protocol, body []byte) (string, error) {
	projectID, _ := p.config.ProviderConf["project_id"].(string)
	region, _ := p.config.ProviderConf["region"].(string)
	if protocol != ai_protocols.OpenAIEmbeddings {
		return fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s/endpoints/openapi/chat/completions",
			region,
			url.PathEscape(projectID),
			url.PathEscape(region),
		), nil
	}
	model := p.requestModel(body)
	if model == "" {
		return "", fmt.Errorf("vertex-ai embeddings requires options.model or request body model")
	}
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		region,
		url.PathEscape(projectID),
		url.PathEscape(region),
		url.PathEscape(model),
	), nil
}

func (p *Plugin) requestModel(body []byte) string {
	if model, _ := p.config.Options["model"].(string); model != "" {
		return model
	}
	return modelFromBody(body)
}

func appendBedrockEndpoint(endpoint string, model string, streaming bool) (string, error) {
	if model == "" {
		return "", fmt.Errorf("bedrock requires options.model or request body model")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse Bedrock endpoint: %w", err)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return endpoint, nil
	}
	suffix := "/converse"
	if streaming {
		suffix = "/converse-stream"
	}
	parsed.Path = "/model/" + model + suffix
	return parsed.String(), nil
}

func appendProtocolEndpoint(endpoint string, protocol ai_protocols.Protocol) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse OpenAI-compatible endpoint: %w", err)
	}
	switch strings.TrimRight(parsed.Path, "/") {
	case "", "/v1":
		parsed.Path = protocol.Endpoint
	default:
		return endpoint, nil
	}
	return parsed.String(), nil
}

func (p *Plugin) writeProviderResponse(
	w http.ResponseWriter,
	r *http.Request,
	prepared preparedProviderRequest,
	started time.Time,
	resp *http.Response,
) {
	if requestIsStreaming(prepared.clientBody, prepared.clientProtocol) {
		for field, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(field, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		flushInterval := time.Duration(*p.config.StreamingFlushIntervalMS) * time.Millisecond
		streamWriter := ai_stream.NewFlushWriter(w, flushInterval, func() {
			ai_runtime.MarkFirstToken(r, started)
		})
		var usage ai_stream.Usage
		var err error
		if prepared.providerProtocol == ai_protocols.BedrockConverse {
			usage, err = ai_stream.ForwardAWSEventStream(streamWriter, resp.Body, p.config.MaxResponseBytes)
		} else if prepared.anthropicConversion {
			usage, err = ai_stream.ForwardOpenAIAsAnthropicSSE(
				streamWriter,
				resp.Body,
				p.config.MaxResponseBytes,
				prepared.toolNameMap,
			)
		} else {
			usage, err = ai_stream.ForwardSSE(
				streamWriter,
				resp.Body,
				prepared.providerProtocol,
				p.config.MaxResponseBytes,
			)
		}
		streamWriter.Close()
		if err == nil {
			registerStreamingLLMRequestVars(r, prepared.clientBody, usage)
		}
		return
	}
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
	ai_runtime.MarkFirstToken(r, started)
	convertedResponse := false
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices &&
		p.config.Provider == "vertex-ai" && prepared.clientProtocol == ai_protocols.OpenAIEmbeddings {
		body, err = ai_protocols.ConvertVertexEmbeddingsToOpenAI(body, p.requestModel(prepared.clientBody))
		if err != nil {
			writeJSONMessage(w, http.StatusBadGateway, err.Error())
			return
		}
		convertedResponse = true
	}
	if prepared.anthropicConversion {
		body, err = ai_protocols.ConvertOpenAIChatToAnthropic(body, "", prepared.toolNameMap)
		if err != nil {
			writeJSONMessage(w, http.StatusBadGateway, err.Error())
			return
		}
		convertedResponse = true
	}
	registerLLMRequestVars(r, prepared.clientBody, prepared.clientProtocol, body)

	for field, values := range resp.Header {
		if convertedResponse && strings.EqualFold(field, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func requestIsStreaming(body []byte, protocol ai_protocols.Protocol) bool {
	var decoded map[string]any
	return json.Unmarshal(body, &decoded) == nil && ai_protocols.IsStreaming(protocol, decoded)
}

func registerStreamingLLMRequestVars(r *http.Request, requestBody []byte, usage ai_stream.Usage) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$request_type", "ai_stream")
	if model := modelFromBody(requestBody); model != "" {
		apisixctx.RegisterRequestVar(r, "$request_llm_model", model)
	}
	if usage.Model != "" {
		apisixctx.RegisterRequestVar(r, "$llm_model", usage.Model)
	}
	if usage.PromptTokens >= 0 {
		apisixctx.RegisterRequestVar(r, "$llm_prompt_tokens", usage.PromptTokens)
	}
	if usage.CompletionTokens >= 0 {
		apisixctx.RegisterRequestVar(r, "$llm_completion_tokens", usage.CompletionTokens)
	}
	if len(usage.Raw) > 0 {
		apisixctx.RegisterRequestVar(r, "$llm_raw_usage", usage.Raw)
	}
	if usage.Text != "" {
		apisixctx.RegisterRequestVar(r, "$llm_response_text", usage.Text)
	}
	if usage.PromptTokens >= 0 && usage.CompletionTokens >= 0 {
		apisixctx.RegisterRequestVar(r, "$ai_token_usage", map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
		})
	}
}

func registerLLMRequestVars(
	r *http.Request,
	requestBody []byte,
	protocol ai_protocols.Protocol,
	responseBody []byte,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}

	requestModel := modelFromBody(requestBody)
	responseMetadata := ai_protocols.ExtractResponseMetadata(protocol, responseBody)
	var decodedResponse map[string]any
	if json.Unmarshal(responseBody, &decodedResponse) == nil {
		apisixctx.RegisterRequestVar(
			r,
			"$llm_response_text",
			ai_protocols.ExtractResponseText(protocol, decodedResponse),
		)
	}

	apisixctx.RegisterRequestVar(r, "$request_type", protocol.RequestType)
	if requestModel != "" {
		apisixctx.RegisterRequestVar(r, "$request_llm_model", requestModel)
	}
	if responseMetadata.Model != "" {
		apisixctx.RegisterRequestVar(r, "$llm_model", responseMetadata.Model)
	}
	if responseMetadata.PromptTokens >= 0 {
		apisixctx.RegisterRequestVar(r, "$llm_prompt_tokens", responseMetadata.PromptTokens)
	}
	if responseMetadata.CompletionTokens >= 0 {
		apisixctx.RegisterRequestVar(r, "$llm_completion_tokens", responseMetadata.CompletionTokens)
	}
	registerUsageContextVars(r, responseBody, responseMetadata.PromptTokens, responseMetadata.CompletionTokens)
}

func registerUsageContextVars(r *http.Request, responseBody []byte, promptTokens, completionTokens int64) {
	var decoded struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(responseBody, &decoded); err != nil || decoded.Usage == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$llm_raw_usage", decoded.Usage)
	if promptTokens < 0 || completionTokens < 0 {
		return
	}
	apisixctx.RegisterRequestVar(r, "$ai_token_usage", map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	})
}

func modelFromBody(body []byte) string {
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ""
	}
	return decoded.Model
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
