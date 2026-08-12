package ai_proxy_multi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
)

type Plugin struct {
	base.BasePlugin
	config    Config
	client    *http.Client
	mu        sync.Mutex
	nextSlot  map[int]int
	selection map[int]*weightSelection
	priority  []int
	instances map[string]int
	now       func() time.Time
	gcpTokens gcpTokenApplier
	healthMu  sync.Mutex
	health    map[int]*instanceHealthState
	healthNow func() time.Time

	healthClients map[int]*http.Client

	healthStopOnce sync.Once
	stoppedHealth  atomic.Bool
	wakeHealth     chan struct{}
	stopHealth     chan struct{}
	healthDone     chan struct{}
	healthCtx      context.Context
	healthCancel   context.CancelFunc
	snapshot       atomic.Pointer[healthSnapshot]
}

type gcpTokenApplier interface {
	Apply(context.Context, *http.Client, *http.Request, ai_auth.GCPConfig) error
}

// weightSelection is the O(1)-lookup weighted index built at configuration
// publication: cumulative weight boundaries over distinct instance IDs. A slot
// in [0, total) maps to an instance via binary search without expanding a
// repeated-provider slice.
type weightSelection struct {
	total      int
	cumulative []int
	ids        []int
}

type preparedInstanceRequest struct {
	clientBody          []byte
	clientDocument      ai_protocols.Document
	providerBody        []byte
	providerDocument    ai_protocols.Document
	clientProtocol      ai_protocols.Protocol
	providerProtocol    ai_protocols.Protocol
	toolNameMap         map[string]string
	anthropicConversion bool
	cancel              context.CancelFunc
	upstreamStarted     time.Time
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
	priority = 1041
	name     = "ai-proxy-multi"
)

var errRequestBodyTooLarge = errors.New("request body exceeds max_req_body_size")

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
          "checks": {
            "type": "object",
            "properties": {
              "active": {
                "type": "object",
                "properties": {
                  "type": {
                    "type": "string",
                    "enum": ["http", "https", "tcp"],
                    "default": "http"
                  },
                  "timeout": {
                    "type": "number",
                    "default": 1
                  },
                  "concurrency": {
                    "type": "integer",
                    "default": 10
                  },
                  "host": {
                    "type": "string",
                    "minLength": 1
                  },
                  "port": {
                    "type": "integer",
                    "minimum": 1,
                    "maximum": 65535
                  },
                  "http_path": {
                    "type": "string",
                    "default": "/"
                  },
                  "https_verify_certificate": {
                    "type": "boolean",
                    "default": true
                  },
                  "healthy": {
                    "type": "object",
                    "properties": {
                      "interval": {
                        "type": "integer",
                        "minimum": 1,
                        "default": 1
                      },
                      "http_statuses": {
                        "$ref": "#/$defs/health_statuses"
                      },
                      "successes": {
                        "type": "integer",
                        "minimum": 1,
                        "maximum": 254,
                        "default": 2
                      }
                    }
                  },
                  "unhealthy": {
                    "type": "object",
                    "properties": {
                      "interval": {
                        "type": "integer",
                        "minimum": 1,
                        "default": 1
                      },
                      "http_statuses": {
                        "$ref": "#/$defs/health_statuses"
                      },
                      "http_failures": {
                        "$ref": "#/$defs/health_failure_threshold"
                      },
                      "tcp_failures": {
                        "$ref": "#/$defs/health_failure_threshold"
                      },
                      "timeouts": {
                        "$ref": "#/$defs/health_failure_threshold"
                      }
                    }
                  },
                  "req_headers": {
                    "type": "array",
                    "minItems": 1,
                    "uniqueItems": true,
                    "items": {
                      "type": "string"
                    }
                  }
                }
              }
            },
            "required": ["active"]
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
  "required": ["instances"],
  "$defs": {
    "auth_items": {
      "type": "object",
      "patternProperties": {
        "^[a-zA-Z0-9._-]+$": {
          "type": "string"
        }
      }
    },
    "health_statuses": {
      "type": "array",
      "minItems": 1,
      "uniqueItems": true,
      "items": {
        "type": "integer",
        "minimum": 200,
        "maximum": 599
      }
    },
    "health_failure_threshold": {
      "type": "integer",
      "minimum": 1,
      "maximum": 254
    }
  }
}
`

type Config struct {
	Balancer                 Balancer   `json:"balancer"`
	Instances                []Instance `json:"instances"`
	Logging                  Logging    `json:"logging"`
	FallbackStrategy         any        `json:"fallback_strategy,omitempty"`
	MaxRetries               *int       `json:"max_retries,omitempty"`
	RetryOnFailureWithinMS   int        `json:"retry_on_failure_within_ms,omitempty"`
	Timeout                  int        `json:"timeout,omitempty"`
	MaxReqBodySize           int64      `json:"max_req_body_size,omitempty"`
	MaxStreamDurationMS      int        `json:"max_stream_duration_ms,omitempty"`
	MaxResponseBytes         int64      `json:"max_response_bytes,omitempty"`
	Keepalive                *bool      `json:"keepalive,omitempty"`
	KeepaliveTimeout         int        `json:"keepalive_timeout,omitempty"`
	KeepalivePool            int        `json:"keepalive_pool,omitempty"`
	StreamingFlushIntervalMS *int       `json:"streaming_flush_interval_ms,omitempty"`
	SSLVerify                *bool      `json:"ssl_verify,omitempty"`
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
	Override     Override       `json:"override"`
	Checks       *HealthChecks  `json:"checks,omitempty"`
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
	if p.config.StreamingFlushIntervalMS == nil {
		flushInterval := 10
		p.config.StreamingFlushIntervalMS = &flushInterval
	}
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}

	p.selection = make(map[int]*weightSelection)
	p.priority = p.priority[:0]
	p.nextSlot = make(map[int]int)
	p.instances = make(map[string]int)
	for i := range p.config.Instances {
		instance := &p.config.Instances[i]
		if instance.Name != "" {
			// First occurrence wins, matching the previous linear scan.
			if _, exists := p.instances[instance.Name]; !exists {
				p.instances[instance.Name] = i
			}
		}
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
		if instance.Provider == "bedrock" {
			if region, _ := instance.ProviderConf["region"].(string); region == "" {
				return fmt.Errorf("instance %q: bedrock requires provider_conf.region", instance.Name)
			}
			if instance.Auth.AWS == nil {
				return fmt.Errorf("instance %q: bedrock requires auth.aws", instance.Name)
			}
		}
		if instance.Provider == "vertex-ai" && instance.Override.Endpoint == "" {
			projectID, _ := instance.ProviderConf["project_id"].(string)
			region, _ := instance.ProviderConf["region"].(string)
			if projectID == "" || region == "" {
				return fmt.Errorf(
					"instance %q: vertex-ai requires provider_conf project_id and region or override.endpoint",
					instance.Name,
				)
			}
		}
		if _, ok := p.selection[instance.Priority]; !ok {
			p.priority = append(p.priority, instance.Priority)
		}
		sel := p.selection[instance.Priority]
		if sel == nil {
			sel = &weightSelection{}
			p.selection[instance.Priority] = sel
		}
		sel.total += instance.Weight
		sel.cumulative = append(sel.cumulative, sel.total)
		sel.ids = append(sel.ids, i)
	}
	if len(p.priority) == 0 {
		return fmt.Errorf("at least one instance must have weight greater than 0")
	}
	sort.Sort(sort.Reverse(sort.IntSlice(p.priority)))

	p.client = &http.Client{Transport: p.transport()}
	if p.now == nil {
		p.now = time.Now
	}
	if p.gcpTokens == nil {
		p.gcpTokens = ai_auth.NewGCPTokenSource()
	}
	if p.healthNow == nil {
		p.healthNow = time.Now
	}
	p.initHealthStates()
	if len(p.health) > 0 {
		p.startHealthLoop()
	}
	return nil
}

// RunRequestPhase parses the client document, selects the initial provider
// instance, and publishes one request-local execution state. Provider I/O is
// deferred to RunExclusiveProtocol until all before-proxy hooks have run.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	body, document, protocol, err := p.readJSONDocument(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errRequestBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		base.WriteJSONMessage(w, status, err.Error())
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	p.refreshHealth(r.Context())
	firstIndex, ok := p.pickInstance(r, nil)
	if !ok {
		base.WriteJSONMessage(w, http.StatusServiceUnavailable, "failed to pick AI instance")
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	tried := map[int]bool{firstIndex: true}
	var state *ai_runtime.State
	request := ai_runtime.WithExecution(r, p.config.Instances[firstIndex].Name, func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		index, ok := p.instanceIndex(state.InstanceName())
		if !ok {
			base.WriteJSONMessage(w, http.StatusServiceUnavailable, "failed to pick AI instance")
			p.registerLogging(r, protocol, body)
			return
		}
		p.executeInstanceRequest(w, r, body, document, protocol, index, tried)
	})
	state = ai_runtime.FromRequest(request)
	state.SetStreaming(p.instanceIsStreaming(body, document, protocol, p.config.Instances[firstIndex]))
	state.ConfigureRateLimitFallback(rateLimitFallbackEnabled(p.config.FallbackStrategy), func() bool {
		index, ok := p.pickInstance(request, tried)
		if !ok {
			return false
		}
		tried[index] = true
		state.SetInstanceName(p.config.Instances[index].Name)
		state.SetStreaming(p.instanceIsStreaming(body, document, protocol, p.config.Instances[index]))
		return true
	})
	return base.ContinueRequest(request)
}

// RunExclusiveProtocol executes the selected AI instance once and marks its
// response as upstream-owned before any provider bytes can be committed.
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

// Handler is retained only for direct callers that have not installed the
// explicit request/response phases. Route assembly uses the interfaces above.
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

// DescribeResponseMode conservatively includes bounded and streaming modes;
// each selected provider instance may choose SSE at request time.
func (*Config) DescribeResponseMode() (base.ResponseModeDescriptor, error) {
	return base.ResponseModeDescriptor{Modes: base.ResponseModeBounded | base.ResponseModeStreaming}, nil
}

func (p *Plugin) instanceIsStreaming(
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
	instance Instance,
) bool {
	_, providerDocument, err := p.providerBody(body, document, protocol, instance)
	return err == nil && providerDocument.IsStreaming(protocol)
}

func (p *Plugin) executeInstanceRequest(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
	firstIndex int,
	tried map[int]bool,
) {
	retries := 0
	index := firstIndex
	for {
		tried[index] = true
		instance := p.config.Instances[index]
		if err := validateBedrockInstanceRequest(instance, document, protocol); err != nil {
			base.WriteJSONMessage(w, http.StatusBadRequest, err.Error())
			p.registerLogging(r, protocol, body)
			return
		}
		p.registerRequestIdentity(r, document, protocol, instance)
		started := ai_runtime.StartLLMRequest(r)
		doneMetric := metrics.BeginLLMRequest(r)

		start := time.Now()
		resp, prepared, err := p.requestInstance(r, body, document, protocol, instance)
		if err != nil {
			doneMetric()
			if prepared.cancel != nil {
				prepared.cancel()
			}
			ai_runtime.MarkLLMRequestDone(r, started)
			if p.canRetry(http.StatusServiceUnavailable, time.Since(start), retries) {
				retries++
				var ok bool
				index, ok = p.pickInstance(r, tried)
				if !ok {
					base.WriteJSONMessage(w, http.StatusServiceUnavailable, "failed to pick AI instance")
					return
				}
				ai_runtime.FromRequest(r).SetInstanceName(p.config.Instances[index].Name)
				continue
			}
			base.WriteJSONMessage(w, http.StatusServiceUnavailable, "failed to request LLM: "+err.Error())
			p.registerLogging(r, protocol, body)
			return
		}

		if p.canRetry(resp.StatusCode, time.Since(start), retries) && len(tried) < len(p.config.Instances) {
			doneMetric()
			ai_runtime.MarkLLMRequestDone(r, started)
			retries++
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if prepared.cancel != nil {
				prepared.cancel()
			}
			var ok bool
			index, ok = p.pickInstance(r, tried)
			if !ok {
				base.WriteJSONMessage(w, http.StatusServiceUnavailable, "failed to pick AI instance")
				p.registerLogging(r, protocol, body)
				return
			}
			ai_runtime.FromRequest(r).SetInstanceName(p.config.Instances[index].Name)
			continue
		}

		defer func() { _ = resp.Body.Close() }()
		responseBody := &countingReadCloser{ReadCloser: resp.Body}
		resp.Body = responseBody
		p.writeProviderResponse(w, r, prepared, instanceModelDocument(instance, document), instance, started, resp)
		registerUpstreamResponseVars(
			r,
			resp.StatusCode,
			time.Since(prepared.upstreamStarted),
			responseBody.bytesRead,
		)
		doneMetric()
		if prepared.cancel != nil {
			prepared.cancel()
		}
		ai_runtime.MarkLLMRequestDone(r, started)
		p.registerLogging(r, protocol, body)
		return
	}
}

func validateBedrockInstanceRequest(
	instance Instance,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
) error {
	if instance.Provider != "bedrock" {
		return nil
	}
	if protocol != ai_protocols.BedrockConverse {
		return fmt.Errorf("bedrock provider does not support %s protocol", protocol.OverrideKey)
	}
	if instanceModelDocument(instance, document) == "" {
		return fmt.Errorf("could not resolve upstream path: bedrock requires options.model or request body model")
	}
	return nil
}

func (p *Plugin) registerRequestIdentity(
	r *http.Request,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
	instance Instance,
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
	if model := instanceModelDocument(instance, document); model != "" {
		apisixctx.RegisterRequestVar(r, "$request_llm_model", model)
		apisixctx.RegisterRequestVar(r, "$llm_model", model)
	}
}

func (p *Plugin) registerLogging(r *http.Request, protocol ai_protocols.Protocol, body []byte) {
	ai_runtime.RegisterLogging(r, p.config.Logging.Summaries, p.config.Logging.Payloads, protocol, body)
}

func (p *Plugin) instanceIndex(name string) (int, bool) {
	index, ok := p.instances[name]
	return index, ok
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
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, fmt.Errorf("missing request body")
	}

	document, err := ai_protocols.DecodeDocument(body)
	if err != nil {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, fmt.Errorf(
			"could not parse JSON request body: %w",
			err,
		)
	}
	protocol, err := ai_protocols.Detect(r.URL.Path, document.Raw)
	if err != nil {
		return nil, ai_protocols.Document{}, ai_protocols.Protocol{}, err
	}
	return body, document, protocol, nil
}

func (p *Plugin) requestInstance(
	r *http.Request,
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
	instance Instance,
) (*http.Response, preparedInstanceRequest, error) {
	prepared, err := p.prepareInstanceRequest(body, document, protocol, instance)
	if err != nil {
		return nil, prepared, err
	}
	registerLLMRequestVars(r, prepared.clientDocument, prepared.clientProtocol, ai_protocols.Document{})

	endpoint, err := p.endpoint(instance, prepared.providerProtocol, prepared.clientDocument)
	if err != nil {
		return nil, prepared, err
	}

	method := http.MethodPost
	if protocol == ai_protocols.Passthrough {
		method = r.Method
	}
	req, err := http.NewRequestWithContext(r.Context(), method, endpoint, bytes.NewReader(prepared.providerBody))
	if err != nil {
		return nil, prepared, fmt.Errorf("failed to create LLM request: %w", err)
	}
	ai_common.CopyForwardHeaders(req.Header, r.Header)
	req.Header.Set("Content-Type", "application/json")
	for header, value := range instance.Auth.Header {
		req.Header.Set(header, value)
	}
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
	for key, value := range instance.Auth.Query {
		query.Set(key, value)
	}
	req.URL.RawQuery = query.Encode()
	registerUpstreamTargetVars(r, req)
	if instance.Auth.GCP != nil {
		if err := p.gcpTokens.Apply(r.Context(), p.client, req, *instance.Auth.GCP); err != nil {
			return nil, prepared, fmt.Errorf("authenticate GCP request: %w", err)
		}
	}
	if instance.Provider == "bedrock" {
		region, _ := instance.ProviderConf["region"].(string)
		if err := ai_auth.SignAWSRequest(
			req,
			prepared.providerBody,
			*instance.Auth.AWS,
			region,
			"bedrock",
			p.now(),
		); err != nil {
			return nil, prepared, fmt.Errorf("sign Bedrock request: %w", err)
		}
	}
	if prepared.anthropicConversion {
		ai_protocols.ConvertAnthropicHeadersToOpenAI(req.Header)
	}
	if prepared.clientDocument.IsStreaming(prepared.clientProtocol) && p.config.MaxStreamDurationMS > 0 {
		deadlineContext, cancel := context.WithTimeout(
			req.Context(),
			time.Duration(p.config.MaxStreamDurationMS)*time.Millisecond,
		)
		prepared.cancel = cancel
		req = req.WithContext(deadlineContext)
	}

	prepared.upstreamStarted = time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		registerUpstreamResponseTime(r, time.Since(prepared.upstreamStarted))
	}
	return resp, prepared, err
}

func (p *Plugin) prepareInstanceRequest(
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
	instance Instance,
) (preparedInstanceRequest, error) {
	prepared := preparedInstanceRequest{
		clientBody:       body,
		clientDocument:   document,
		providerDocument: document,
		clientProtocol:   protocol,
		providerProtocol: protocol,
	}
	if protocol != ai_protocols.AnthropicMessages || !ai_protocols.ProviderUsesOpenAIChat(instance.Provider) {
		providerBody, providerDocument, err := p.providerBody(body, document, protocol, instance)
		prepared.providerBody = providerBody
		prepared.providerDocument = providerDocument
		return prepared, err
	}
	clientBody := maps.Clone(document.Raw)
	for key, value := range instance.Options {
		clientBody[key] = ai_common.CloneJSONValue(value)
	}
	convertedDocument, toolNameMap, err := ai_protocols.ConvertAnthropicMessagesDocumentToOpenAI(
		ai_protocols.Document{Raw: clientBody},
	)
	if err != nil {
		return prepared, fmt.Errorf("convert Anthropic request to OpenAI Chat: %w", err)
	}
	convertedBody := convertedDocument.Raw
	p.applyLLMOptions(convertedBody, ai_protocols.OpenAIChat, instance)
	p.applyRequestBodyOverride(convertedBody, ai_protocols.OpenAIChat, instance)
	p.applyProviderBodyRules(convertedBody, instance)
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

func (p *Plugin) providerBody(
	body []byte,
	document ai_protocols.Document,
	protocol ai_protocols.Protocol,
	instance Instance,
) ([]byte, ai_protocols.Document, error) {
	// Copy only the mutated request fields: a shallow top-level copy isolates
	// the provider document from the shared client document, and nested maps
	// are cloned on write (MergeBodyMap, bedrock inference config) instead of
	// deep-copying the whole payload on every request.
	bodyTab := maps.Clone(document.Raw)
	changed := false
	for key, value := range instance.Options {
		if !ai_common.JSONValueEqual(bodyTab[key], value) {
			changed = true
		}
		bodyTab[key] = ai_common.CloneJSONValue(value)
	}
	if p.applyLLMOptions(bodyTab, protocol, instance) {
		changed = true
	}
	if p.applyRequestBodyOverride(bodyTab, protocol, instance) {
		changed = true
	}
	if p.applyProviderBodyRules(bodyTab, instance) {
		changed = true
	}
	providerDocument := ai_protocols.Document{Raw: bodyTab}
	if providerDocument.IsStreaming(protocol) && protocol == ai_protocols.OpenAIChat {
		streamOptions := map[string]any{"include_usage": true}
		if !ai_common.JSONValueEqual(bodyTab["stream_options"], streamOptions) {
			changed = true
		}
		bodyTab["stream_options"] = streamOptions
	}

	vertexEmbeddings := instance.Provider == "vertex-ai" && protocol == ai_protocols.OpenAIEmbeddings
	if !changed && !vertexEmbeddings {
		return body, providerDocument, nil
	}
	rewritten, err := json.Marshal(bodyTab)
	if err != nil {
		return nil, ai_protocols.Document{}, fmt.Errorf("failed to encode provider request body: %w", err)
	}
	if vertexEmbeddings {
		converted, err := ai_protocols.ConvertOpenAIEmbeddingsToVertex(rewritten)
		if err != nil {
			return nil, ai_protocols.Document{}, err
		}
		convertedDocument, err := ai_protocols.DecodeDocument(converted)
		return converted, convertedDocument, err
	}
	return rewritten, providerDocument, nil
}

func (p *Plugin) applyRequestBodyOverride(
	body map[string]any,
	protocol ai_protocols.Protocol,
	instance Instance,
) bool {
	override := requestBodyOverride(instance.Override.RequestBody, protocol)
	if len(override) == 0 {
		return false
	}
	force := instance.Override.RequestBodyForceOverride != nil && *instance.Override.RequestBodyForceOverride
	return ai_common.MergeBodyMap(body, override, force)
}

func requestBodyOverride(values map[string]any, protocol ai_protocols.Protocol) map[string]any {
	if len(values) == 0 {
		return nil
	}
	if override, ok := ai_common.AsAnyMap(values[protocol.OverrideKey]); ok {
		return override
	}
	if ai_common.HasProtocolRequestBodyOverride(values) {
		return nil
	}
	if protocol != ai_protocols.OpenAIChat {
		return nil
	}
	return values
}

func (p *Plugin) applyProviderBodyRules(body map[string]any, instance Instance) bool {
	changed := false
	if instance.Provider == "azure-openai" || instance.Provider == "bedrock" {
		if _, ok := body["model"]; ok {
			delete(body, "model")
			changed = true
		}
	}
	if instance.Provider == "bedrock" {
		if _, ok := body["stream"]; ok {
			delete(body, "stream")
			changed = true
		}
	}
	return changed
}

func (p *Plugin) applyLLMOptions(body map[string]any, protocol ai_protocols.Protocol, instance Instance) bool {
	if instance.Override.LLMOptions.MaxTokens == 0 {
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
	switch instance.Provider {
	case "openai":
		switch protocol {
		case ai_protocols.OpenAIChat:
			set("max_completion_tokens", instance.Override.LLMOptions.MaxTokens)
			remove("max_tokens")
		case ai_protocols.OpenAIResponses:
			set("max_output_tokens", instance.Override.LLMOptions.MaxTokens)
		}
	case "gemini", "vertex-ai":
		if protocol == ai_protocols.OpenAIChat {
			set("max_completion_tokens", instance.Override.LLMOptions.MaxTokens)
		}
	case "bedrock":
		if protocol == ai_protocols.BedrockConverse {
			inferenceConfig, ok := body["inferenceConfig"].(map[string]any)
			if !ok {
				inferenceConfig = make(map[string]any)
				body["inferenceConfig"] = inferenceConfig
				changed = true
			} else {
				// clone-on-write: never mutate a nested map shared with the
				// client document under a shallow top-level copy
				inferenceConfig = ai_common.CloneJSONValue(inferenceConfig).(map[string]any)
				body["inferenceConfig"] = inferenceConfig
			}
			if !ai_common.JSONValueEqual(inferenceConfig["maxTokens"], instance.Override.LLMOptions.MaxTokens) {
				changed = true
			}
			inferenceConfig["maxTokens"] = instance.Override.LLMOptions.MaxTokens
		}
	default:
		if protocol == ai_protocols.OpenAIChat {
			set("max_tokens", instance.Override.LLMOptions.MaxTokens)
		}
	}
	return changed
}

func fallbackStrategyHas(strategy any, name string) bool {
	switch values := strategy.(type) {
	case string:
		return values == name
	case []string:
		if slices.Contains(values, name) {
			return true
		}
	case []any:
		if slices.ContainsFunc(values, func(value any) bool { return value == name }) {
			return true
		}
	}
	return false
}

func rateLimitFallbackEnabled(strategy any) bool {
	return strategy == "instance_health_and_rate_limiting" || fallbackStrategyHas(strategy, "rate_limiting")
}

func (p *Plugin) endpoint(
	instance Instance,
	protocol ai_protocols.Protocol,
	document ai_protocols.Document,
) (string, error) {
	if instance.Override.Endpoint != "" {
		if protocol == ai_protocols.Passthrough {
			return instance.Override.Endpoint, nil
		}
		if instance.Provider == "openai-compatible" || instance.Provider == "openai" {
			return ai_protocols.AppendProtocolEndpoint(instance.Override.Endpoint, protocol)
		}
		if instance.Provider == "bedrock" {
			return ai_protocols.AppendBedrockEndpoint(
				instance.Override.Endpoint,
				instanceModelDocument(instance, document),
				document.IsStreaming(protocol),
			)
		}
		return instance.Override.Endpoint, nil
	}

	switch instance.Provider {
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
		region, _ := instance.ProviderConf["region"].(string)
		return ai_protocols.AppendBedrockEndpoint(
			"https://bedrock-runtime."+region+".amazonaws.com",
			instanceModelDocument(instance, document),
			document.IsStreaming(protocol),
		)
	case "vertex-ai":
		return vertexEndpoint(instance, protocol, document)
	default:
		return "", fmt.Errorf("provider %q requires override.endpoint in apisix-go", instance.Provider)
	}
}

func vertexEndpoint(
	instance Instance,
	protocol ai_protocols.Protocol,
	document ai_protocols.Document,
) (string, error) {
	projectID, _ := instance.ProviderConf["project_id"].(string)
	region, _ := instance.ProviderConf["region"].(string)
	if protocol != ai_protocols.OpenAIEmbeddings {
		return fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s/endpoints/openapi/chat/completions",
			region,
			url.PathEscape(projectID),
			url.PathEscape(region),
		), nil
	}
	model := instanceModelDocument(instance, document)
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

func instanceModelDocument(instance Instance, document ai_protocols.Document) string {
	if model, _ := instance.Options["model"].(string); model != "" {
		return model
	}
	return document.Model()
}

func (p *Plugin) writeProviderResponse(
	w http.ResponseWriter,
	r *http.Request,
	prepared preparedInstanceRequest,
	requestModel string,
	instance Instance,
	started time.Time,
	resp *http.Response,
) {
	if prepared.clientDocument.IsStreaming(prepared.clientProtocol) {
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
			registerStreamingLLMRequestVars(r, prepared.clientDocument, usage)
		}
		return
	}
	bodyReader := io.Reader(resp.Body)
	if p.config.MaxResponseBytes > 0 {
		bodyReader = io.LimitReader(resp.Body, p.config.MaxResponseBytes+1)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		base.WriteJSONMessage(w, http.StatusBadGateway, "failed to read LLM response body: "+err.Error())
		return
	}
	if p.config.MaxResponseBytes > 0 && int64(len(body)) > p.config.MaxResponseBytes {
		base.WriteJSONMessage(w, http.StatusBadGateway, "max_response_bytes exceeded")
		return
	}
	ai_runtime.MarkFirstToken(r, started)
	convertedResponse := false
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices &&
		instance.Provider == "vertex-ai" && prepared.clientProtocol == ai_protocols.OpenAIEmbeddings {
		body, err = ai_protocols.ConvertVertexEmbeddingsToOpenAI(body, requestModel)
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
	if requestModel != "" && apisixctx.GetRequestVars(r) != nil {
		apisixctx.RegisterRequestVar(r, "$request_llm_model", requestModel)
	}

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

func registerStreamingLLMRequestVars(
	r *http.Request,
	requestDocument ai_protocols.Document,
	usage ai_stream.Usage,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	ai_stream.RegisterStreamingLLMRequestVars(r, requestDocument, usage)
}

func registerLLMRequestVars(
	r *http.Request,
	requestDocument ai_protocols.Document,
	protocol ai_protocols.Protocol,
	responseDocument ai_protocols.Document,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}

	ai_protocols.RegisterLLMRequestVars(r, requestDocument, protocol, responseDocument)
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	ai_common.ApplyTransportKeepalive(transport, p.config.KeepalivePool, p.config.KeepaliveTimeout, p.config.Keepalive)
	ai_common.ApplyTransportSSLVerify(transport, p.config.SSLVerify)
	timeout := time.Duration(p.config.Timeout) * time.Millisecond
	transport.DialContext = (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = timeout
	transport.ResponseHeaderTimeout = timeout
	return pxy.NewProgressTimeoutTransport(transport, timeout, timeout)
}
