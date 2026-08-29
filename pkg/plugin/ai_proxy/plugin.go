package ai_proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config    Config
	client    *http.Client
	now       func() time.Time
	gcpTokens gcpTokenApplier
	secrets   aiProxySecretState

	streamOutcomeRecorded func()
}

type gcpTokenApplier interface {
	Apply(context.Context, *http.Client, *http.Request, ai_auth.GCPConfig) error
}

type preparedProviderRequest struct {
	clientBody          []byte
	clientDocument      ai_protocols.Document
	providerBody        []byte
	providerDocument    ai_protocols.Document
	clientProtocol      ai_protocols.Protocol
	providerProtocol    ai_protocols.Protocol
	toolNameMap         map[string]string
	anthropicConversion bool
}

type countingReadCloser struct {
	io.ReadCloser
	bytesRead int64
}

func (r *countingReadCloser) Read(body []byte) (int, error) {
	read, err := r.ReadCloser.Read(body)
	r.bytesRead += int64(read)
	return read, err
}

const (
	priority = 1040
	name     = "ai-proxy"
)

var (
	errInvalidClientRequest = errors.New("invalid client request")
	errRequestBodyEmpty     = errors.New("could not get body: request body is empty")
	errRequestBodyTooLarge  = errors.New("request body exceeds max_req_body_size")
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
          "properties": {
            "openai-chat": {
              "type": "object",
              "additionalProperties": true
            },
            "openai-responses": {
              "type": "object",
              "additionalProperties": true
            },
            "openai-embeddings": {
              "type": "object",
              "additionalProperties": true
            },
            "anthropic-messages": {
              "type": "object",
              "additionalProperties": true
            },
            "bedrock-converse": {
              "type": "object",
              "additionalProperties": true
            },
            "passthrough": {
              "type": "object",
              "additionalProperties": true
            }
          },
          "additionalProperties": false
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

	p.client = &http.Client{Transport: p.transport()}
	if p.now == nil {
		p.now = time.Now
	}
	if p.gcpTokens == nil {
		p.gcpTokens = ai_auth.NewGCPTokenSource()
	}
	return nil
}

// RunRequestPhase validates and prepares the request-local AI execution. It
// never contacts the provider or invokes a downstream handler; the explicit
// protocol owner consumes the published operation after before-proxy hooks.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	body, document, protocol, err := p.readJSONDocument(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errRequestBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
			logger.Errorf("failed to read request body: %v", err)
		}
		if errors.Is(err, errRequestBodyEmpty) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, util.BuildMessageResponse(err.Error())+"\n")
		} else {
			base.WriteJSONMessage(w, status, err.Error())
		}
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	if err := p.validateProviderRequest(document, protocol); err != nil {
		base.WriteJSONMessage(w, http.StatusBadRequest, err.Error())
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	request := ai_runtime.WithExecution(r, "ai-proxy-"+p.config.Provider, func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		p.executeProviderRequest(w, r, body, document, protocol)
	})
	ai_runtime.FromRequest(request).SetStreamingIntent(document.IsStreaming(protocol))
	return base.ContinueRequest(request)
}

// RunExclusiveProtocol consumes the prepared provider operation exactly once.
// AI responses are upstream-owned, so the response source is selected before
// the provider operation can write, flush, or return a streaming body.
func (p *Plugin) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) (base.ProtocolDisposition, *http.Request, apisixctx.ResponseSource, error) {
	state := ai_runtime.FromRequest(r)
	if state == nil {
		if next != nil {
			next.ServeHTTP(w, r)
		}
		return base.ProtocolResponded, r, apisixctx.ResponseSourceUnknown, nil
	}
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
	state.Consume(w, r)
	return base.ProtocolResponded, r, apisixctx.ResponseSourceUpstream, nil
}

// Handler remains a narrow compatibility seam for callers that have not yet
// installed the request/terminal phases. The route pipeline uses the explicit
// interfaces above and never enters this legacy next-aware path.
func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := p.RunRequestPhase(w, r)
		request := result.Request
		if request == nil {
			request = r
		}
		if result.Decision == base.RequestStop {
			return
		}
		if ai_runtime.TerminalEnabled(request) {
			if next != nil {
				next.ServeHTTP(w, request)
			}
			return
		}
		_, _, _, _ = p.RunExclusiveProtocol(w, request, next)
	})
}

// DescribeResponseMode conservatively advertises both bounded and streaming
// responses because the request document selects SSE at runtime.
func (*Config) DescribeResponseMode() (base.ResponseModeDescriptor, error) {
	return base.ResponseModeDescriptor{Modes: base.ResponseModeBounded | base.ResponseModeStreaming}, nil
}

func (p *Plugin) validateProviderRequest(document ai_protocols.Document, protocol ai_protocols.Protocol) error {
	if p.config.Provider != "bedrock" {
		return nil
	}
	if protocol != ai_protocols.BedrockConverse {
		return fmt.Errorf("bedrock provider does not support %s protocol", protocol.OverrideKey)
	}
	if p.requestModelDocument(document) == "" {
		return fmt.Errorf("could not resolve upstream path: bedrock requires options.model or request body model")
	}
	return nil
}

func (p *Plugin) executeProviderRequest(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
) {
	p.registerRequestIdentity(r, document, protocol)
	started := ai_runtime.StartLLMRequest(r)
	defer func() {
		ai_runtime.MarkLLMRequestDone(r, started)
		ai_runtime.RegisterLogging(r, p.config.Logging.Summaries, p.config.Logging.Payloads, protocol, body)
	}()
	doneMetric := metrics.BeginLLMRequest(r)
	defer doneMetric()
	prepared, err := p.prepareProviderRequest(body, document, protocol)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errInvalidClientRequest) {
			status = http.StatusBadRequest
		}
		base.WriteJSONMessage(w, status, err.Error())
		return
	}
	proxyReq, err := p.buildProviderRequestDocument(
		r,
		prepared.providerBody,
		prepared.providerDocument,
		prepared.providerProtocol,
	)
	if err != nil {
		base.WriteJSONMessage(w, http.StatusBadGateway, err.Error())
		return
	}
	if prepared.anthropicConversion {
		ai_protocols.ConvertAnthropicHeadersToOpenAI(proxyReq.Header)
	}
	if prepared.clientDocument.IsStreaming(prepared.clientProtocol) && p.config.MaxStreamDurationMS > 0 {
		deadlineContext, cancel := context.WithTimeout(
			proxyReq.Context(),
			time.Duration(p.config.MaxStreamDurationMS)*time.Millisecond,
		)
		defer cancel()
		proxyReq = proxyReq.WithContext(deadlineContext)
	}
	upstreamStarted := time.Now()
	registerUpstreamTargetVars(r, proxyReq)
	resp, err := p.client.Do(proxyReq)
	if err != nil {
		registerUpstreamResponseTime(r, time.Since(upstreamStarted))
		base.WriteJSONMessage(w, providerRequestErrorStatus(err), "failed to request LLM")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody := &countingReadCloser{ReadCloser: resp.Body}
	resp.Body = responseBody
	p.writeProviderResponse(w, r, prepared, started, resp)
	registerUpstreamResponseVars(
		r,
		resp.StatusCode,
		time.Since(upstreamStarted),
		responseBody.bytesRead,
	)
}

func (p *Plugin) registerRequestIdentity(
	r *http.Request,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	requestType := protocol.RequestType
	if document.IsStreaming(protocol) {
		requestType = "ai_stream"
	}
	apisixctx.RegisterRequestVar(r, "$request_type", requestType)
	apisixctx.RegisterRequestVar(r, "$llm_request_body", document.Raw)
	if model := p.requestModelDocument(document); model != "" {
		apisixctx.RegisterRequestVar(r, "$request_llm_model", model)
		apisixctx.RegisterRequestVar(r, "$llm_model", model)
	}
}

func (p *Plugin) prepareProviderRequest(
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
) (preparedProviderRequest, error) {
	prepared := preparedProviderRequest{
		clientBody:       body,
		clientDocument:   document,
		providerBody:     body,
		providerDocument: document,
		clientProtocol:   protocol,
		providerProtocol: protocol,
	}
	if protocol != ai_protocols.AnthropicMessages || !ai_protocols.ProviderUsesOpenAIChat(p.config.Provider) {
		return prepared, nil
	}
	convertedDocument, toolNameMap, err := ai_protocols.ConvertAnthropicMessagesDocumentToOpenAI(document)
	if err != nil {
		return prepared, fmt.Errorf("%w: convert Anthropic request to OpenAI Chat: %v", errInvalidClientRequest, err)
	}
	convertedBody := convertedDocument.Raw
	p.applyLLMOptions(convertedBody, ai_protocols.OpenAIChat)
	p.applyRequestBodyOverride(convertedBody, ai_protocols.OpenAIChat)
	p.applyProviderBodyRules(convertedBody)
	if ai_protocols.IsStreaming(ai_protocols.OpenAIChat, convertedBody) {
		convertedBody["stream_options"] = map[string]any{"include_usage": true}
	}
	converted, err := json.Marshal(convertedBody)
	if err != nil {
		return prepared, fmt.Errorf("encode converted OpenAI Chat request: %w", err)
	}
	prepared.providerBody = converted
	prepared.providerDocument = ai_protocols.Document{Raw: convertedBody}
	prepared.providerProtocol = ai_protocols.OpenAIChat
	prepared.toolNameMap = toolNameMap
	prepared.anthropicConversion = true
	return prepared, nil
}

func (p *Plugin) readJSONDocument(r *http.Request) ([]byte, ai_protocols.Document, ai_protocols.Protocol, error) {
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, fmt.Errorf(
			"unsupported content-type: %s, only application/json is supported",
			contentType,
		)
	}
	if r.ContentLength > p.config.MaxReqBodySize {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, errRequestBodyTooLarge
	}

	reader := io.LimitReader(r.Body, p.config.MaxReqBodySize+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, fmt.Errorf("could not get body: %w", err)
	}
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, fmt.Errorf("could not get body: %w", err)
	}
	if int64(len(body)) > p.config.MaxReqBodySize {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, errRequestBodyTooLarge
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, errRequestBodyEmpty
	}

	document, err := ai_protocols.DecodeDocument(body)
	if err != nil {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, fmt.Errorf(
			"could not parse JSON request body: %w",
			err,
		)
	}
	bodyTab := document.Raw
	changed := false
	protocol, err := ai_protocols.Detect(r.URL.Path, bodyTab)
	if err != nil {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, err
	}
	for key, value := range p.config.Options {
		if !ai_common.JSONValueEqual(bodyTab[key], value) {
			changed = true
		}
		bodyTab[key] = ai_common.CloneJSONValue(value)
	}
	if protocol != ai_protocols.AnthropicMessages || !ai_protocols.ProviderUsesOpenAIChat(p.config.Provider) {
		if p.applyLLMOptions(bodyTab, protocol) {
			changed = true
		}
		if document.IsStreaming(protocol) && protocol == ai_protocols.OpenAIChat {
			streamOptions := map[string]any{"include_usage": true}
			if !ai_common.JSONValueEqual(bodyTab["stream_options"], streamOptions) {
				changed = true
			}
			bodyTab["stream_options"] = streamOptions
		}
		if p.applyRequestBodyOverride(bodyTab, protocol) {
			changed = true
		}
		if p.applyProviderBodyRules(bodyTab) {
			changed = true
		}
	}
	if !changed {
		return body, document, protocol, nil
	}

	rewritten, err := json.Marshal(bodyTab)
	if err != nil {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, fmt.Errorf(
			"failed to encode provider request body: %w",
			err,
		)
	}
	return rewritten, document, protocol, nil
}

func (p *Plugin) applyRequestBodyOverride(body map[string]any, protocol ai_protocols.Protocol) bool {
	override := p.requestBodyOverride(protocol)
	if len(override) == 0 {
		return false
	}
	force := p.config.Override.RequestBodyForceOverride != nil && *p.config.Override.RequestBodyForceOverride
	return ai_common.MergeBodyMap(body, override, force)
}

func (p *Plugin) requestBodyOverride(protocol ai_protocols.Protocol) map[string]any {
	if len(p.config.Override.RequestBody) == 0 {
		return nil
	}
	if override, ok := ai_common.AsAnyMap(p.config.Override.RequestBody[protocol.OverrideKey]); ok {
		return override
	}
	if ai_common.HasProtocolRequestBodyOverride(p.config.Override.RequestBody) {
		return nil
	}
	if protocol != ai_protocols.OpenAIChat {
		return nil
	}
	return p.config.Override.RequestBody
}

func (p *Plugin) applyProviderBodyRules(body map[string]any) bool {
	if p.config.Provider == "azure-openai" {
		if _, ok := body["model"]; ok {
			delete(body, "model")
			return true
		}
	}
	return false
}

func (p *Plugin) applyLLMOptions(body map[string]any, protocol ai_protocols.Protocol) bool {
	if p.config.Override.LLMOptions.MaxTokens == 0 {
		return false
	}
	if protocol == ai_protocols.OpenAIEmbeddings {
		return false
	}
	changed := false
	set := func(key string, value any) {
		if !ai_common.JSONValueEqual(body[key], value) {
			changed = true
		}
		body[key] = value
	}
	remove := func(key string) {
		if _, ok := body[key]; ok {
			delete(body, key)
			changed = true
		}
	}
	switch p.config.Provider {
	case "openai":
		switch protocol {
		case ai_protocols.OpenAIChat:
			set("max_completion_tokens", p.config.Override.LLMOptions.MaxTokens)
			remove("max_tokens")
		case ai_protocols.OpenAIResponses:
			set("max_output_tokens", p.config.Override.LLMOptions.MaxTokens)
		}
	case "openai-compatible":
		switch protocol {
		case ai_protocols.OpenAIChat:
			set("max_tokens", p.config.Override.LLMOptions.MaxTokens)
		case ai_protocols.OpenAIResponses:
			set("max_output_tokens", p.config.Override.LLMOptions.MaxTokens)
		}
	case "gemini", "vertex-ai":
		if protocol == ai_protocols.OpenAIChat {
			set("max_completion_tokens", p.config.Override.LLMOptions.MaxTokens)
		}
	case "bedrock":
		if protocol == ai_protocols.BedrockConverse {
			inferenceConfig, ok := body["inferenceConfig"].(map[string]any)
			if !ok {
				inferenceConfig = make(map[string]any)
				body["inferenceConfig"] = inferenceConfig
				changed = true
			}
			if !ai_common.JSONValueEqual(inferenceConfig["maxTokens"], p.config.Override.LLMOptions.MaxTokens) {
				changed = true
			}
			inferenceConfig["maxTokens"] = p.config.Override.LLMOptions.MaxTokens
		}
	default:
		if protocol == ai_protocols.OpenAIChat {
			set("max_tokens", p.config.Override.LLMOptions.MaxTokens)
		}
	}
	return changed
}

func (p *Plugin) buildProviderRequest(
	r *http.Request,
	body []byte,
	protocol ai_protocols.Protocol,
) (*http.Request, error) {
	document, err := ai_protocols.DecodeDocument(body)
	if err != nil {
		return nil, err
	}
	return p.buildProviderRequestDocument(r, body, document, protocol)
}

func (p *Plugin) buildProviderRequestDocument(
	r *http.Request,
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
) (*http.Request, error) {
	endpoint, err := p.endpointDocument(protocol, document)
	if err != nil {
		return nil, err
	}
	providerBody, err := p.finalProviderBody(body, document, protocol)
	if err != nil {
		return nil, err
	}

	method := http.MethodPost
	if protocol == ai_protocols.Passthrough {
		method = r.Method
	}
	req, err := http.NewRequestWithContext(r.Context(), method, endpoint, bytes.NewReader(providerBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM request: %w", err)
	}
	ai_common.CopyForwardHeaders(req.Header, r.Header)
	req.Header.Set("Content-Type", "application/json")
	query := req.URL.Query()
	if protocol == ai_protocols.Passthrough {
		if req.URL.Path == "" || req.URL.Path == "/" {
			req.URL.Path = r.URL.Path
			req.URL.RawPath = r.URL.RawPath
		}
		for key, values := range r.URL.Query() {
			query.Del(key)
			for _, value := range values {
				query.Add(key, value)
			}
		}
	}
	if err := p.withAuth(func(auth Auth) error {
		for header, value := range auth.Header {
			req.Header.Set(header, value)
		}
		for key, value := range auth.Query {
			apisixctx.RegisterSensitiveQueryName(r, key)
			query.Set(key, value)
		}
		req.URL.RawQuery = query.Encode()
		if auth.GCP != nil {
			if err := p.gcpTokens.Apply(r.Context(), p.client, req, *auth.GCP); err != nil {
				return fmt.Errorf("authenticate GCP request: %w", err)
			}
		}
		if p.config.Provider == "bedrock" {
			region, _ := p.config.ProviderConf["region"].(string)
			if err := ai_auth.SignAWSRequest(
				req,
				providerBody,
				*auth.AWS,
				region,
				"bedrock",
				p.now(),
			); err != nil {
				return fmt.Errorf("sign Bedrock request: %w", err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return req, nil
}

func (p *Plugin) finalProviderBody(
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
) ([]byte, error) {
	if p.config.Provider == "vertex-ai" && protocol == ai_protocols.OpenAIEmbeddings {
		return ai_protocols.ConvertOpenAIEmbeddingsToVertex(body)
	}
	if p.config.Provider != "bedrock" || protocol != ai_protocols.BedrockConverse {
		return body, nil
	}
	decoded := maps.Clone(document.Raw)
	delete(decoded, "model")
	delete(decoded, "stream")
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("encode Bedrock request body: %w", err)
	}
	return encoded, nil
}

func (p *Plugin) writeProviderResponse(
	w http.ResponseWriter,
	r *http.Request,
	prepared preparedProviderRequest,
	started time.Time,
	resp *http.Response,
) {
	if isProviderErrorStatus(resp.StatusCode) {
		w.WriteHeader(resp.StatusCode)
		return
	}
	if prepared.clientDocument.IsStreaming(prepared.clientProtocol) {
		for field, values := range resp.Header {
			if prepared.anthropicConversion && strings.EqualFold(field, "Content-Length") {
				continue
			}
			for _, value := range values {
				w.Header().Add(field, value)
			}
		}
		flushInterval := time.Duration(*p.config.StreamingFlushIntervalMS) * time.Millisecond
		streamWriter := ai_stream.NewFlushWriter(r.Context(), w, flushInterval, func() {
			ai_runtime.MarkFirstToken(r, started)
		})
		defer ai_stream.ClosePreservingPanic(streamWriter)
		streamWriter.WriteHeader(resp.StatusCode)
		var usage ai_stream.Usage
		var err error
		transport := ai_stream.StreamTransportSSE
		if prepared.providerProtocol == ai_protocols.BedrockConverse {
			transport = ai_stream.StreamTransportAWSEventStream
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
		outcome := ai_stream.RecordStreamOutcome(r, transport, err)
		if p.streamOutcomeRecorded != nil {
			p.streamOutcomeRecorded()
		}
		if err != nil {
			wrote := streamWriter.Wrote()
			if outcome == ai_stream.StreamOutcomeCanceled {
				if errors.Is(err, context.DeadlineExceeded) ||
					strings.Contains(err.Error(), "context deadline exceeded") {
					logger.Errorf("aborting AI stream: max_stream_duration_ms exceeded")
					return
				}
				logger.Warnf("client disconnected during AI streaming")
				return
			}
			if !wrote {
				logger.Errorf("%v", err)
				clear(w.Header())
				message := "failed to forward streaming response"
				if errors.Is(err, ai_stream.ErrNoStreamOutput) {
					message = err.Error()
				}
				base.WriteJSONMessage(w, http.StatusBadGateway, message)
				return
			}
			if terminalErr := ai_stream.WriteTerminalError(streamWriter, transport); terminalErr != nil {
				logger.Warnf("failed to write AI stream terminal event: %v", terminalErr)
			}
			logger.Errorf("failed to forward streaming response: %v", err)
			return
		}
		if !streamWriter.Wrote() {
			w.WriteHeader(resp.StatusCode)
		}
		registerStreamingLLMRequestVars(r, prepared.clientDocument, usage)
		return
	}
	bodyReader := io.Reader(resp.Body)
	if p.config.MaxResponseBytes > 0 && resp.ContentLength > p.config.MaxResponseBytes {
		logger.Errorf(
			"aborting AI response: Content-Length %d exceeds max_response_bytes %d",
			resp.ContentLength,
			p.config.MaxResponseBytes,
		)
		base.WriteJSONMessage(w, http.StatusBadGateway, "max_response_bytes exceeded")
		return
	}
	if p.config.MaxResponseBytes > 0 {
		bodyReader = io.LimitReader(resp.Body, p.config.MaxResponseBytes+1)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		base.WriteJSONMessage(w, http.StatusBadGateway, "failed to read LLM response body: "+err.Error())
		return
	}
	if p.config.MaxResponseBytes > 0 && int64(len(body)) > p.config.MaxResponseBytes {
		logger.Errorf(
			"aborting AI response: body size exceeds max_response_bytes %d",
			p.config.MaxResponseBytes,
		)
		base.WriteJSONMessage(w, http.StatusBadGateway, "max_response_bytes exceeded")
		return
	}
	ai_runtime.MarkFirstToken(r, started)
	convertedResponse := false
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices &&
		p.config.Provider == "vertex-ai" && prepared.clientProtocol == ai_protocols.OpenAIEmbeddings {
		body, err = ai_protocols.ConvertVertexEmbeddingsToOpenAI(body, p.requestModel(prepared.clientBody))
		if err != nil {
			base.WriteJSONMessage(w, http.StatusBadGateway, err.Error())
			return
		}
		convertedResponse = true
	}
	if prepared.anthropicConversion {
		body, err = ai_protocols.ConvertOpenAIChatToAnthropic(body, "", prepared.toolNameMap)
		if err != nil {
			base.WriteJSONMessage(w, http.StatusBadGateway, err.Error())
			return
		}
		convertedResponse = true
	}
	responseDocument, _ := ai_protocols.DecodeDocument(body)
	registerLLMRequestVars(r, prepared.clientDocument, prepared.clientProtocol, responseDocument)

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

func isProviderErrorStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= http.StatusInternalServerError && status < 600)
}

func providerRequestErrorStatus(err error) int {
	if isTimeoutError(err) {
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var timeoutErr net.Error
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	return false
}
