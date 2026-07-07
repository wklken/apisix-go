package ai_proxy_multi

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	client   *http.Client
	mu       sync.Mutex
	nextSlot int
	weighted []int
}

const (
	priority = 1041
	name     = "ai-proxy-multi"
)

const schema = `
{
  "type": "object",
  "properties": {
    "balancer": {
      "type": "object",
      "properties": {
        "algorithm": {
          "type": "string",
          "enum": ["chash", "roundrobin"],
          "default": "roundrobin"
        },
        "hash_on": {
          "type": "string",
          "enum": ["vars", "header", "cookie", "consumer", "vars_combinations"],
          "default": "vars"
        },
        "key": {
          "type": "string"
        }
      },
      "default": {
        "algorithm": "roundrobin"
      }
    },
    "instances": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "name": {
            "type": "string",
            "minLength": 1
          },
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
          "priority": {
            "type": "integer",
            "default": 0
          },
          "weight": {
            "type": "integer",
            "minimum": 0
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
          "checks": {
            "type": "object"
          }
        },
        "required": ["name", "provider", "weight", "auth"]
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
    "fallback_strategy": {
      "anyOf": [
        {
          "type": "string",
          "enum": ["instance_health_and_rate_limiting", "http_429", "http_5xx"]
        },
        {
          "type": "array",
          "items": {
            "type": "string",
            "enum": ["rate_limiting", "http_429", "http_5xx"]
          }
        }
      ]
    },
    "max_retries": {
      "type": "integer",
      "minimum": 0
    },
    "retry_on_failure_within_ms": {
      "type": "integer",
      "minimum": 1
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
    "ssl_verify": {
      "type": "boolean",
      "default": true
    }
  },
  "required": ["instances"],
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
	Balancer               Balancer   `json:"balancer,omitempty"`
	Instances              []Instance `json:"instances"`
	Logging                Logging    `json:"logging,omitempty"`
	FallbackStrategy       any        `json:"fallback_strategy,omitempty"`
	MaxRetries             *int       `json:"max_retries,omitempty"`
	RetryOnFailureWithinMS int        `json:"retry_on_failure_within_ms,omitempty"`
	Timeout                int        `json:"timeout,omitempty"`
	MaxReqBodySize         int64      `json:"max_req_body_size,omitempty"`
	MaxResponseBytes       int64      `json:"max_response_bytes,omitempty"`
	Keepalive              *bool      `json:"keepalive,omitempty"`
	KeepaliveTimeout       int        `json:"keepalive_timeout,omitempty"`
	KeepalivePool          int        `json:"keepalive_pool,omitempty"`
	SSLVerify              *bool      `json:"ssl_verify,omitempty"`
}

type Balancer struct {
	Algorithm string `json:"algorithm,omitempty"`
	HashOn    string `json:"hash_on,omitempty"`
	Key       string `json:"key,omitempty"`
}

type Instance struct {
	Name         string         `json:"name"`
	Provider     string         `json:"provider"`
	ProviderConf map[string]any `json:"provider_conf,omitempty"`
	Priority     int            `json:"priority,omitempty"`
	Weight       int            `json:"weight"`
	Auth         Auth           `json:"auth"`
	Options      map[string]any `json:"options,omitempty"`
	Override     Override       `json:"override,omitempty"`
	Checks       map[string]any `json:"checks,omitempty"`
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
	if len(p.config.Instances) == 0 {
		return fmt.Errorf("instances is required")
	}
	if p.config.Balancer.Algorithm == "" {
		p.config.Balancer.Algorithm = "roundrobin"
	}
	if p.config.Balancer.Algorithm == "chash" {
		if p.config.Balancer.HashOn == "" {
			return fmt.Errorf("must configure hash_on when balancer algorithm is chash")
		}
		if p.config.Balancer.HashOn != "consumer" && p.config.Balancer.Key == "" {
			return fmt.Errorf("must configure key when balancer hash_on is not consumer")
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
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}

	p.weighted = p.weighted[:0]
	for i := range p.config.Instances {
		instance := &p.config.Instances[i]
		if instance.Weight == 0 {
			continue
		}
		if (instance.Provider == "openai-compatible" || instance.Provider == "azure-openai") &&
			instance.Override.Endpoint == "" {
			return fmt.Errorf(
				"instance %q: override.endpoint is required for %s provider",
				instance.Name,
				instance.Provider,
			)
		}
		for range instance.Weight {
			p.weighted = append(p.weighted, i)
		}
	}
	if len(p.weighted) == 0 {
		return fmt.Errorf("at least one instance must have weight greater than 0")
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

		tried := make(map[int]bool, len(p.config.Instances))
		retries := 0
		for {
			index, ok := p.pickInstance(r, tried)
			if !ok {
				writeJSONMessage(w, http.StatusServiceUnavailable, "failed to pick AI instance")
				return
			}
			tried[index] = true
			instance := p.config.Instances[index]

			start := time.Now()
			resp, err := p.requestInstance(r, body, instance)
			if err != nil {
				if p.canRetry(http.StatusServiceUnavailable, time.Since(start), retries) {
					retries++
					continue
				}
				writeJSONMessage(w, http.StatusServiceUnavailable, "failed to request LLM: "+err.Error())
				return
			}

			if p.canRetry(resp.StatusCode, time.Since(start), retries) && len(tried) < len(p.config.Instances) {
				retries++
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				continue
			}

			defer resp.Body.Close()
			p.writeProviderResponse(w, resp)
			return
		}
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

	rewritten, err := json.Marshal(bodyTab)
	if err != nil {
		return nil, fmt.Errorf("failed to encode provider request body: %w", err)
	}
	return rewritten, nil
}

func (p *Plugin) requestInstance(r *http.Request, body []byte, instance Instance) (*http.Response, error) {
	providerBody, err := p.providerBody(body, instance)
	if err != nil {
		return nil, err
	}

	endpoint, err := p.endpoint(instance)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(providerBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM request: %w", err)
	}
	copyForwardHeaders(req.Header, r.Header)
	req.Header.Set("Content-Type", "application/json")
	for header, value := range instance.Auth.Header {
		req.Header.Set(header, value)
	}
	query := req.URL.Query()
	for key, value := range instance.Auth.Query {
		query.Set(key, value)
	}
	req.URL.RawQuery = query.Encode()

	return p.client.Do(req)
}

func (p *Plugin) providerBody(body []byte, instance Instance) ([]byte, error) {
	var bodyTab map[string]any
	if err := json.Unmarshal(body, &bodyTab); err != nil {
		return nil, fmt.Errorf("could not parse JSON request body: %w", err)
	}
	for key, value := range instance.Options {
		bodyTab[key] = value
	}
	p.applyLLMOptions(bodyTab, instance)

	rewritten, err := json.Marshal(bodyTab)
	if err != nil {
		return nil, fmt.Errorf("failed to encode provider request body: %w", err)
	}
	return rewritten, nil
}

func (p *Plugin) applyLLMOptions(body map[string]any, instance Instance) {
	if instance.Override.LLMOptions.MaxTokens == 0 {
		return
	}
	switch instance.Provider {
	case "openai":
		body["max_completion_tokens"] = instance.Override.LLMOptions.MaxTokens
		delete(body, "max_tokens")
	default:
		body["max_tokens"] = instance.Override.LLMOptions.MaxTokens
	}
}

func (p *Plugin) pickInstance(r *http.Request, tried map[int]bool) (int, bool) {
	if len(p.weighted) == 0 {
		return 0, false
	}

	start := p.nextWeightedSlot(r)
	for offset := range len(p.weighted) {
		index := p.weighted[(start+offset)%len(p.weighted)]
		if !tried[index] {
			return index, true
		}
	}
	return 0, false
}

func (p *Plugin) nextWeightedSlot(r *http.Request) int {
	if p.config.Balancer.Algorithm == "chash" {
		key := p.hashKey(r)
		hasher := fnv.New32a()
		_, _ = hasher.Write([]byte(key))
		return int(hasher.Sum32() % uint32(len(p.weighted)))
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	slot := p.nextSlot % len(p.weighted)
	p.nextSlot++
	return slot
}

func (p *Plugin) hashKey(r *http.Request) string {
	switch p.config.Balancer.HashOn {
	case "header":
		return r.Header.Get(p.config.Balancer.Key)
	case "cookie":
		cookie, err := r.Cookie(p.config.Balancer.Key)
		if err == nil {
			return cookie.Value
		}
		return ""
	case "vars", "vars_combinations":
		switch p.config.Balancer.Key {
		case "uri", "request_uri":
			return r.URL.RequestURI()
		case "remote_addr":
			return r.RemoteAddr
		default:
			return p.config.Balancer.Key
		}
	default:
		return p.config.Balancer.Key
	}
}

func (p *Plugin) canRetry(code int, elapsed time.Duration, retries int) bool {
	if p.config.MaxRetries != nil && retries >= *p.config.MaxRetries {
		return false
	}
	if p.config.RetryOnFailureWithinMS > 0 &&
		elapsed > time.Duration(p.config.RetryOnFailureWithinMS)*time.Millisecond {
		return false
	}
	if code == http.StatusTooManyRequests {
		return fallbackStrategyHas(p.config.FallbackStrategy, "http_429")
	}
	return code >= http.StatusInternalServerError &&
		code < http.StatusNetworkAuthenticationRequired &&
		fallbackStrategyHas(p.config.FallbackStrategy, "http_5xx")
}

func fallbackStrategyHas(strategy any, name string) bool {
	switch values := strategy.(type) {
	case string:
		return values == name
	case []string:
		for _, value := range values {
			if value == name {
				return true
			}
		}
	case []any:
		for _, value := range values {
			if value == name {
				return true
			}
		}
	}
	return false
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

func (p *Plugin) endpoint(instance Instance) (string, error) {
	if instance.Override.Endpoint != "" {
		return instance.Override.Endpoint, nil
	}

	switch instance.Provider {
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
		return "", fmt.Errorf("provider %q requires override.endpoint in apisix-go", instance.Provider)
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
