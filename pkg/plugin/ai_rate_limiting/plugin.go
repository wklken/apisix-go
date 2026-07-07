package ai_rate_limiting

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu       sync.Mutex
	counters map[string]*counter
	now      func() time.Time
}

const (
	priority = 1030
	name     = "ai-rate-limiting"
)

const schema = `
{
  "type": "object",
  "properties": {
    "limit": {
      "type": "integer",
      "exclusiveMinimum": 0
    },
    "time_window": {
      "type": "integer",
      "exclusiveMinimum": 0
    },
    "show_limit_quota_header": {
      "type": "boolean",
      "default": true
    },
    "limit_strategy": {
      "type": "string",
      "enum": ["total_tokens", "prompt_tokens", "completion_tokens", "expression"],
      "default": "total_tokens"
    },
    "cost_expr": {
      "type": "string",
      "minLength": 1
    },
    "instances": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "name": {
            "type": "string"
          },
          "limit": {
            "type": "integer",
            "minimum": 1
          },
          "time_window": {
            "type": "integer",
            "minimum": 1
          }
        },
        "required": ["name", "limit", "time_window"]
      }
    },
    "rejected_code": {
      "type": "integer",
      "minimum": 200,
      "maximum": 599,
      "default": 503
    },
    "rejected_msg": {
      "type": "string",
      "minLength": 1
    },
    "rules": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "count": {
            "type": "integer",
            "exclusiveMinimum": 0
          },
          "time_window": {
            "type": "integer",
            "exclusiveMinimum": 0
          },
          "key": {
            "type": "string"
          },
          "header_prefix": {
            "type": "string"
          }
        },
        "required": ["count", "time_window", "key"]
      }
    }
  },
  "dependencies": {
    "limit": ["time_window"],
    "time_window": ["limit"]
  },
  "oneOf": [
    {
      "anyOf": [
        {
          "required": ["limit", "time_window"]
        },
        {
          "required": ["instances"]
        }
      ]
    },
    {
      "required": ["rules"]
    }
  ]
}
`

type Config struct {
	Limit                int64           `json:"limit,omitempty"`
	TimeWindow           int64           `json:"time_window,omitempty"`
	ShowLimitQuotaHeader *bool           `json:"show_limit_quota_header,omitempty"`
	LimitStrategy        string          `json:"limit_strategy,omitempty"`
	CostExpr             string          `json:"cost_expr,omitempty"`
	Instances            []InstanceLimit `json:"instances,omitempty"`
	RejectedCode         int             `json:"rejected_code,omitempty"`
	RejectedMsg          string          `json:"rejected_msg,omitempty"`
	Rules                []Rule          `json:"rules,omitempty"`
}

type InstanceLimit struct {
	Name       string `json:"name"`
	Limit      int64  `json:"limit"`
	TimeWindow int64  `json:"time_window"`
}

type Rule struct {
	Count        int64  `json:"count"`
	TimeWindow   int64  `json:"time_window"`
	Key          string `json:"key"`
	HeaderPrefix string `json:"header_prefix,omitempty"`
}

type quota struct {
	key        string
	headerName string
	limit      int64
	window     time.Duration
}

type counter struct {
	used  int64
	reset time.Time
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

type pickedInstanceKey struct{}

func WithPickedAIInstanceName(r *http.Request, name string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), pickedInstanceKey{}, name))
}

func PickedAIInstanceName(r *http.Request) (string, bool) {
	name, ok := r.Context().Value(pickedInstanceKey{}).(string)
	return name, ok && name != ""
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
	if p.config.ShowLimitQuotaHeader == nil {
		show := true
		p.config.ShowLimitQuotaHeader = &show
	}
	if p.config.LimitStrategy == "" {
		p.config.LimitStrategy = "total_tokens"
	}
	if p.config.LimitStrategy == "expression" {
		return fmt.Errorf("limit_strategy expression is not supported")
	}
	if len(p.config.Rules) > 0 {
		return fmt.Errorf("rules are not supported")
	}
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = http.StatusServiceUnavailable
	}
	if p.config.RejectedCode < 200 || p.config.RejectedCode > 599 {
		return fmt.Errorf("rejected_code must be between 200 and 599")
	}
	if len(p.config.Instances) == 0 && (p.config.Limit <= 0 || p.config.TimeWindow <= 0) {
		return fmt.Errorf("limit and time_window must be greater than 0")
	}
	for _, instance := range p.config.Instances {
		if instance.Name == "" {
			return fmt.Errorf("instance name is required")
		}
		if instance.Limit <= 0 || instance.TimeWindow <= 0 {
			return fmt.Errorf("instance %s limit and time_window must be greater than 0", instance.Name)
		}
	}

	if p.counters == nil {
		p.counters = map[string]*counter{}
	}
	if p.now == nil {
		p.now = time.Now
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		q, ok := p.quotaForRequest(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		if !p.allowed(q) {
			p.writeQuotaHeaders(w.Header(), q)
			p.reject(w)
			return
		}

		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)
		usedTokens := p.responseTokenCost(recorder.body.Bytes())
		if usedTokens > 0 {
			p.charge(q, usedTokens)
		}
		p.writeQuotaHeaders(recorder.header, q)
		recorder.writeTo(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) quotaForRequest(r *http.Request) (quota, bool) {
	instanceName, hasInstance := PickedAIInstanceName(r)
	if hasInstance {
		for _, instance := range p.config.Instances {
			if instance.Name == instanceName {
				return quota{
					key:        "instance:" + instance.Name,
					headerName: instance.Name,
					limit:      instance.Limit,
					window:     time.Duration(instance.TimeWindow) * time.Second,
				}, true
			}
		}
		if len(p.config.Instances) > 0 {
			return quota{}, false
		}
		return quota{
			key:        "global",
			headerName: instanceName,
			limit:      p.config.Limit,
			window:     time.Duration(p.config.TimeWindow) * time.Second,
		}, true
	}

	if len(p.config.Instances) > 0 {
		return quota{}, false
	}
	return quota{
		key:        "global",
		headerName: "global",
		limit:      p.config.Limit,
		window:     time.Duration(p.config.TimeWindow) * time.Second,
	}, true
}

func (p *Plugin) allowed(q quota) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.state(q)
	return state.used < q.limit
}

func (p *Plugin) charge(q quota, tokens int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.state(q)
	state.used += tokens
}

func (p *Plugin) state(q quota) *counter {
	now := p.now()
	state, ok := p.counters[q.key]
	if !ok || !now.Before(state.reset) {
		state = &counter{reset: now.Add(q.window)}
		p.counters[q.key] = state
	}
	return state
}

func (p *Plugin) responseTokenCost(body []byte) int64 {
	var decoded struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Usage == nil {
		return 0
	}

	value := numericUsage(decoded.Usage[p.config.LimitStrategy])
	if value < 0 {
		return 0
	}
	return value
}

func numericUsage(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(math.Round(typed))
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

func (p *Plugin) writeQuotaHeaders(header http.Header, q quota) {
	if p.config.ShowLimitQuotaHeader != nil && !*p.config.ShowLimitQuotaHeader {
		return
	}

	used, reset := p.snapshot(q)
	remaining := q.limit - used
	if remaining < 0 {
		remaining = 0
	}
	header.Set("X-AI-RateLimit-Limit-"+q.headerName, strconv.FormatInt(q.limit, 10))
	header.Set("X-AI-RateLimit-Remaining-"+q.headerName, strconv.FormatInt(remaining, 10))
	header.Set("X-AI-RateLimit-Reset-"+q.headerName, strconv.FormatInt(reset.Unix(), 10))
}

func (p *Plugin) snapshot(q quota) (int64, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.state(q)
	return state.used, state.reset
}

func (p *Plugin) reject(w http.ResponseWriter) {
	if p.config.RejectedMsg == "" {
		http.Error(w, http.StatusText(p.config.RejectedCode), p.config.RejectedCode)
		return
	}
	http.Error(w, p.config.RejectedMsg, p.config.RejectedCode)
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = w.Write(r.body.Bytes())
}
