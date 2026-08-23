package ai_rate_limiting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/casbin/govaluate"
	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"github.com/wklken/apisix-go/pkg/resource"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
)

type Plugin struct {
	base.BasePlugin
	config Config

	counters *cacheutil.BoundedTTLMap[counter]
	now      func() time.Time
	costExpr *govaluate.EvaluableExpression
	redis    redisClient

	resourceScope  string
	configIdentity string
}

// redisClient is the subset of the go-redis API the plugin uses, so tests can
// inject a counting fake that asserts request context propagation and round
// trips per decision.
type redisClient interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
	Close() error
}

const (
	priority = 1030
	name     = "ai-rate-limiting"
)

var (
	variablePattern      = regexp.MustCompile(`\$\{?[A-Za-z0-9_]+\}?`)
	quotaVariablePattern = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)(?:\s*\?\?\s*([^}]+))?\}|\$([A-Za-z0-9_]+)`)
)

var (
	errNoUsableRules     = errors.New("no usable rate limit rules")
	errQuotaStateMissing = errors.New("AI rate limit quota is exhausted before response accounting")
)

const redisReserveScript = `
-- apisix-go AI quota reservation
local cost = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local current = tonumber(redis.call("GET", KEYS[1]) or 0)
local ttl = redis.call("PTTL", KEYS[1])
if ttl < 0 then
  current = 0
end
if cost > limit - current then
  return 0
end
if ttl < 0 then
  redis.call("SET", KEYS[1], current + cost, "PX", window)
else
  redis.call("INCRBY", KEYS[1], cost)
end
return 1
`

const redisReconcileScript = `
-- apisix-go AI quota response reconciliation
local delta = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local create = tonumber(ARGV[4])
local ttl = redis.call("PTTL", KEYS[1])
if ttl < 0 and create ~= 1 then
  return 0
end
local current = tonumber(redis.call("GET", KEYS[1]) or 0)
local next = math.max(0, math.min(limit, current + delta))
if ttl < 0 then
  redis.call("SET", KEYS[1], next, "PX", window)
else
  redis.call("SET", KEYS[1], next, "KEEPTTL")
end
return next
`

const redisSnapshotScript = `
local value = redis.call("GET", KEYS[1])
local ttl = redis.call("PTTL", KEYS[1])
return {value, ttl}
`

const schema = `
{
  "type": "object",
  "properties": {
    "limit": {
      "oneOf": [
        {"type": "integer", "exclusiveMinimum": 0},
        {"type": "string"}
      ]
    },
    "time_window": {
      "oneOf": [
        {"type": "integer", "exclusiveMinimum": 0},
        {"type": "string"}
      ]
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
            "oneOf": [
              {"type": "integer", "minimum": 1},
              {"type": "string"}
            ]
          },
          "time_window": {
            "oneOf": [
              {"type": "integer", "minimum": 1},
              {"type": "string"}
            ]
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
            "oneOf": [
              {"type": "integer", "exclusiveMinimum": 0},
              {"type": "string"}
            ]
          },
          "time_window": {
            "oneOf": [
              {"type": "integer", "exclusiveMinimum": 0},
              {"type": "string"}
            ]
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
    },
    "policy": {"type": "string", "enum": ["local", "redis", "redis-sentinel"], "default": "local"},
    "redis_host": {"type": "string", "minLength": 1},
    "redis_port": {"type": "integer", "minimum": 1, "default": 6379},
    "redis_username": {"type": "string"},
    "redis_password": {"type": "string"},
    "redis_database": {"type": "integer", "minimum": 0, "default": 0},
    "redis_timeout": {"type": "integer", "minimum": 1, "default": 1000},
    "redis_sentinels": {"type": "array", "minItems": 1, "items": {"type": "object", "properties": {"host": {"type": "string", "minLength": 1}, "port": {"type": "integer", "minimum": 1}}, "required": ["host", "port"]}},
    "redis_master_name": {"type": "string", "minLength": 1},
    "sentinel_username": {"type": "string"},
    "sentinel_password": {"type": "string"}
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
  ],
  "allOf": [
    {"if": {"properties": {"policy": {"const": "redis"}}, "required": ["policy"]}, "then": {"required": ["redis_host"]}},
    {"if": {"properties": {"policy": {"const": "redis-sentinel"}}, "required": ["policy"]}, "then": {"required": ["redis_sentinels", "redis_master_name"]}}
  ]
}
`

type Config struct {
	Limit                any             `json:"limit,omitempty"`
	TimeWindow           any             `json:"time_window,omitempty"`
	ShowLimitQuotaHeader *bool           `json:"show_limit_quota_header,omitempty"`
	LimitStrategy        string          `json:"limit_strategy,omitempty"`
	CostExpr             string          `json:"cost_expr,omitempty"`
	Instances            []InstanceLimit `json:"instances,omitempty"`
	RejectedCode         int             `json:"rejected_code,omitempty"`
	RejectedMsg          string          `json:"rejected_msg,omitempty"`
	Rules                []Rule          `json:"rules,omitempty"`
	Policy               string          `json:"policy,omitempty"`
	RedisHost            string          `json:"redis_host,omitempty"`
	RedisPort            int             `json:"redis_port,omitempty"`
	RedisUsername        string          `json:"redis_username,omitempty"`
	RedisPassword        string          `json:"redis_password,omitempty"`
	RedisDatabase        int             `json:"redis_database,omitempty"`
	RedisTimeout         int             `json:"redis_timeout,omitempty"`
	RedisSentinels       []RedisSentinel `json:"redis_sentinels,omitempty"`
	RedisMasterName      string          `json:"redis_master_name,omitempty"`
	SentinelUsername     string          `json:"sentinel_username,omitempty"`
	SentinelPassword     string          `json:"sentinel_password,omitempty"`
}

type RedisSentinel struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type InstanceLimit struct {
	Name       string `json:"name"`
	Limit      any    `json:"limit"`
	TimeWindow any    `json:"time_window"`
}

type Rule struct {
	Count        any    `json:"count"`
	TimeWindow   any    `json:"time_window"`
	Key          string `json:"key"`
	HeaderPrefix string `json:"header_prefix,omitempty"`
}

type quota struct {
	key          string
	headerName   string
	headerPrefix string
	limit        int64
	window       time.Duration
}

type counter struct {
	used  int64
	reset time.Time
}

type quotaResponseWriter struct {
	http.ResponseWriter
	plugin      *Plugin
	request     *http.Request
	state       *requestQuotaState
	wroteHeader bool
}

type requestQuotaState struct {
	mu      sync.Mutex
	quotas  []quota
	charged bool
}

type requestQuotaStateKey struct{}

func WithPickedAIInstanceName(r *http.Request, name string) *http.Request {
	return ai_runtime.WithSelectedInstanceName(r, name)
}

func PickedAIInstanceName(r *http.Request) (string, bool) {
	return ai_runtime.SelectedInstanceName(r)
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
	if !p.DataEncryption().Configured() {
		return errors.New("data-encryption resolver is required")
	}
	if len(p.config.Rules) > 0 && (len(p.config.Instances) > 0 || p.config.Limit != nil || p.config.TimeWindow != nil) {
		return errors.New("rules cannot be configured with limit, time_window, or instances")
	}
	if len(p.config.Instances) > 0 && ((p.config.Limit == nil) != (p.config.TimeWindow == nil)) {
		return errors.New("limit and time_window must be configured together")
	}
	if p.config.ShowLimitQuotaHeader == nil {
		show := true
		p.config.ShowLimitQuotaHeader = &show
	}
	if p.config.LimitStrategy == "" {
		p.config.LimitStrategy = "total_tokens"
	}
	if p.config.Policy == "" {
		p.config.Policy = "local"
	}
	if p.config.RedisTimeout == 0 {
		p.config.RedisTimeout = 1000
	}
	if p.config.Policy == "redis" {
		if p.config.RedisHost == "" {
			return errors.New("redis_host is required when policy is redis")
		}
		if p.config.RedisPort == 0 {
			p.config.RedisPort = 6379
		}
		password, err := p.resolveSecret("redis_password", p.config.RedisPassword)
		if err != nil {
			return err
		}
		p.redis = redis.NewClient(&redis.Options{
			Addr:         net.JoinHostPort(p.config.RedisHost, strconv.Itoa(p.config.RedisPort)),
			Username:     p.config.RedisUsername,
			Password:     password,
			DB:           p.config.RedisDatabase,
			DialTimeout:  time.Duration(p.config.RedisTimeout) * time.Millisecond,
			ReadTimeout:  time.Duration(p.config.RedisTimeout) * time.Millisecond,
			WriteTimeout: time.Duration(p.config.RedisTimeout) * time.Millisecond,
		})
	}
	if p.config.Policy == "redis-sentinel" {
		if len(p.config.RedisSentinels) == 0 || p.config.RedisMasterName == "" {
			return errors.New("redis_sentinels and redis_master_name are required when policy is redis-sentinel")
		}
		addresses := make([]string, 0, len(p.config.RedisSentinels))
		for _, sentinel := range p.config.RedisSentinels {
			addresses = append(addresses, net.JoinHostPort(sentinel.Host, strconv.Itoa(sentinel.Port)))
		}
		password, err := p.resolveSecret("redis_password", p.config.RedisPassword)
		if err != nil {
			return err
		}
		sentinelPassword, err := p.resolveSecret("sentinel_password", p.config.SentinelPassword)
		if err != nil {
			return err
		}
		p.redis = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       p.config.RedisMasterName,
			SentinelAddrs:    addresses,
			Username:         p.config.RedisUsername,
			Password:         password,
			SentinelUsername: p.config.SentinelUsername,
			SentinelPassword: sentinelPassword,
			DB:               p.config.RedisDatabase,
			DialTimeout:      time.Duration(p.config.RedisTimeout) * time.Millisecond,
			ReadTimeout:      time.Duration(p.config.RedisTimeout) * time.Millisecond,
			WriteTimeout:     time.Duration(p.config.RedisTimeout) * time.Millisecond,
		})
	}
	if p.config.LimitStrategy == "expression" {
		if p.config.CostExpr == "" {
			return fmt.Errorf("cost_expr is required when limit_strategy is expression")
		}
		costExpr, err := govaluate.NewEvaluableExpressionWithFunctions(
			strings.ReplaceAll(p.config.CostExpr, "math.", ""),
			costExpressionFunctions(),
		)
		if err != nil {
			return fmt.Errorf("invalid cost_expr: %w", err)
		}
		p.costExpr = costExpr
	}
	for i, rule := range p.config.Rules {
		if rule.Key == "" {
			return fmt.Errorf("rule %d key is required", i+1)
		}
		if _, err := staticQuotaValue(rule.Count, fmt.Sprintf("rule %d count", i+1)); err != nil {
			return err
		}
		if _, err := staticQuotaValue(rule.TimeWindow, fmt.Sprintf("rule %d time_window", i+1)); err != nil {
			return err
		}
	}
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = http.StatusServiceUnavailable
	}
	if p.config.RejectedCode < 200 || p.config.RejectedCode > 599 {
		return fmt.Errorf("rejected_code must be between 200 and 599")
	}
	if len(p.config.Rules) == 0 && len(p.config.Instances) == 0 {
		if _, err := staticQuotaValue(p.config.Limit, "limit"); err != nil {
			return err
		}
		if _, err := staticQuotaValue(p.config.TimeWindow, "time_window"); err != nil {
			return err
		}
	}
	for _, instance := range p.config.Instances {
		if instance.Name == "" {
			return fmt.Errorf("instance name is required")
		}
		if _, err := staticQuotaValue(instance.Limit, "instance "+instance.Name+" limit"); err != nil {
			return err
		}
		if _, err := staticQuotaValue(instance.TimeWindow, "instance "+instance.Name+" time_window"); err != nil {
			return err
		}
	}

	if p.now == nil {
		p.now = time.Now
	}
	if p.counters == nil {
		p.counters = cacheutil.NewBoundedTTLMap[counter](100000, p.now)
	}
	p.refreshConfigIdentity()
	return nil
}

func (p *Plugin) resolveSecret(field, value string) (string, error) {
	resolved, err := p.DataEncryption().ResolveForContext(value, "ai-rate-limiting."+field)
	if err != nil {
		return "", fmt.Errorf("ai-rate-limiting %s: %w", field, err)
	}
	return resolved, nil
}

func (p *Plugin) SetResourceContext(route resource.Route, service resource.Service) {
	p.resourceScope = fmt.Sprintf(
		"resource:%d:%s:%d:%s",
		len(route.ID),
		route.ID,
		len(service.ID),
		service.ID,
	)
}

func (p *Plugin) refreshConfigIdentity() {
	identity := struct {
		Limit         any             `json:"limit,omitempty"`
		TimeWindow    any             `json:"time_window,omitempty"`
		LimitStrategy string          `json:"limit_strategy"`
		CostExpr      string          `json:"cost_expr,omitempty"`
		Instances     []InstanceLimit `json:"instances,omitempty"`
		Rules         []Rule          `json:"rules,omitempty"`
		Policy        string          `json:"policy"`
	}{
		Limit:         p.config.Limit,
		TimeWindow:    p.config.TimeWindow,
		LimitStrategy: p.config.LimitStrategy,
		CostExpr:      p.config.CostExpr,
		Instances:     p.config.Instances,
		Rules:         p.config.Rules,
		Policy:        p.config.Policy,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		p.configIdentity = ""
		return
	}
	sum := sha256.Sum256(encoded)
	p.configIdentity = hex.EncodeToString(sum[:])
}

func (p *Plugin) Stop() {
	if p.redis != nil {
		_ = p.redis.Close()
	}
}

// RunRequestPhase performs the admission check once after authentication and
// publishes immutable request-local quota bindings for response accounting.
// It does not wrap or invoke the downstream handler.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	var quotas []quota
	for {
		var ok bool
		var err error
		quotas, ok, err = p.quotasForRequest(r)
		if err != nil {
			http.Error(w, "failed to get rate limit rules", http.StatusInternalServerError)
			return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
		}
		if !ok {
			return base.ContinueRequest(r)
		}
		rejectedIndex := p.reserveQuotas(r.Context(), quotas)
		if rejectedIndex < 0 {
			break
		}
		state := ai_runtime.FromRequest(r)
		if len(p.config.Rules) == 0 && state != nil && state.RateLimitFallbackEnabled() &&
			state.AdvanceRateLimitTarget() {
			continue
		}
		for _, headerQuota := range quotas[:rejectedIndex+1] {
			p.writeQuotaHeaders(r.Context(), w.Header(), headerQuota)
		}
		p.reject(w)
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	requestState := &requestQuotaState{quotas: append([]quota(nil), quotas...)}
	request := r.WithContext(context.WithValue(r.Context(), requestQuotaStateKey{}, requestState))
	return base.ContinueRequest(request)
}

func requestQuotaStateFromRequest(r *http.Request) *requestQuotaState {
	if r == nil {
		return nil
	}
	state, _ := r.Context().Value(requestQuotaStateKey{}).(*requestQuotaState)
	return state
}

// SelectResponseMode chooses the one response accounting path after AI request
// preparation has published whether the selected operation is streaming.
func (*Plugin) SelectResponseMode(r *http.Request) base.RequestResponseMode {
	if state := ai_runtime.FromRequest(r); state != nil && state.StreamingIntent() {
		return base.RequestResponseModeStreaming
	}
	return base.RequestResponseModeBounded
}

// WrapStreamingResponse installs request-local quota headers and completion
// accounting. The returned writer owns exactly-once finalization; no quota
// counters are kept on the plugin instance for a live request.
func (p *Plugin) WrapStreamingResponse(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, error) {
	state, err := p.responseQuotaState(r)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return w, nil
	}
	return &quotaResponseWriter{ResponseWriter: w, plugin: p, request: r, state: state}, nil
}

// RunBufferedBodyFilter accounts bounded responses after Plan 15 has produced
// the canonical body and before the final response is committed.
func (p *Plugin) RunBufferedBodyFilter(r *http.Request, response *base.ResponseState) error {
	if response == nil {
		return nil
	}
	state, err := p.responseQuotaState(r)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	for _, q := range state.quotasForResponse(p, r) {
		p.writeQuotaHeaders(r.Context(), response.Header, q)
	}
	state.chargeOnce(p, r, response.Body)
	return nil
}

func (p *Plugin) responseQuotaState(r *http.Request) (*requestQuotaState, error) {
	if state := requestQuotaStateFromRequest(r); state != nil {
		return state, nil
	}
	quotas, ok, err := p.quotasForRequest(r)
	if err != nil || !ok {
		return nil, err
	}
	if p.reserveQuotas(r.Context(), quotas) >= 0 {
		return nil, errQuotaStateMissing
	}
	return &requestQuotaState{quotas: append([]quota(nil), quotas...)}, nil
}

func (s *requestQuotaState) quotasForResponse(p *Plugin, r *http.Request) []quota {
	if s == nil {
		return nil
	}
	if len(p.config.Rules) == 0 {
		if quotas, ok, err := p.quotasForRequest(r); err == nil && ok {
			return quotas
		}
	}
	return append([]quota(nil), s.quotas...)
}

func (s *requestQuotaState) chargeOnce(p *Plugin, r *http.Request, body []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.charged {
		s.mu.Unlock()
		return
	}
	s.charged = true
	s.mu.Unlock()
	usedTokens := p.responseTokenCostForRequest(r, body)
	finalQuotas := s.quotasForResponse(p, r)
	if sameQuotaSet(s.quotas, finalQuotas) {
		for _, q := range s.quotas {
			p.reconcile(r.Context(), q, usedTokens-1, false)
		}
		return
	}
	for _, q := range s.quotas {
		p.reconcile(r.Context(), q, -1, false)
	}
	if usedTokens <= 0 {
		return
	}
	for _, q := range finalQuotas {
		p.reconcile(r.Context(), q, usedTokens, true)
	}
}

func sameQuotaSet(left []quota, right []quota) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		result := p.RunRequestPhase(w, r)
		if result.Decision != base.RequestContinue {
			return
		}
		request := result.Request
		if request == nil {
			request = r
		}
		state := requestQuotaStateFromRequest(request)
		if state == nil {
			next.ServeHTTP(w, request)
			return
		}
		if runtimeState := ai_runtime.FromRequest(request); runtimeState != nil && runtimeState.StreamingIntent() {
			writer := &quotaResponseWriter{ResponseWriter: w, plugin: p, request: request, state: state}
			next.ServeHTTP(writer, request)
			writer.writeQuotaHeaders()
			state.chargeOnce(p, request, nil)
			return
		}

		recorder := base.GetOrCreateTransformResponseWriter(request)
		next.ServeHTTP(recorder, request)
		for _, q := range state.quotasForResponse(p, request) {
			p.writeQuotaHeaders(request.Context(), recorder.Header(), q)
		}
		state.chargeOnce(p, request, recorder.Body())
		recorder.Commit(w)
	}
	return http.HandlerFunc(fn)
}

func (w *quotaResponseWriter) WriteHeader(statusCode int) {
	w.writeQuotaHeaders()
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *quotaResponseWriter) Write(body []byte) (int, error) {
	w.writeQuotaHeaders()
	return w.ResponseWriter.Write(body)
}

func (w *quotaResponseWriter) Flush() {
	w.writeQuotaHeaders()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *quotaResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *quotaResponseWriter) FinishStreamingResponse(error) error {
	if w == nil || w.plugin == nil {
		return nil
	}
	w.writeQuotaHeaders()
	if w.state != nil {
		w.state.chargeOnce(w.plugin, w.request, nil)
	}
	return nil
}

func (w *quotaResponseWriter) writeQuotaHeaders() {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	var quotas []quota
	if w.state != nil {
		quotas = w.state.quotasForResponse(w.plugin, w.request)
	} else {
		var ok bool
		var err error
		quotas, ok, err = w.plugin.quotasForRequest(w.request)
		if err != nil || !ok {
			return
		}
	}
	if len(quotas) == 0 {
		return
	}
	for _, q := range quotas {
		w.plugin.writeQuotaHeaders(w.request.Context(), w.Header(), q)
	}
}

// DescribeResponseMode advertises both bounded and streaming accounting; the
// actual response owner selects the mode per request.
func (*Config) DescribeResponseMode() (base.ResponseModeDescriptor, error) {
	return base.ResponseModeDescriptor{Modes: base.ResponseModeBounded | base.ResponseModeStreaming}, nil
}

func (p *Plugin) reserveQuotas(ctx context.Context, quotas []quota) int {
	for i, q := range quotas {
		if !p.reserve(ctx, q, 1) {
			for _, reserved := range quotas[:i] {
				p.reconcile(ctx, reserved, -1, false)
			}
			return i
		}
	}
	return -1
}

func (p *Plugin) quotasForRequest(r *http.Request) ([]quota, bool, error) {
	if len(p.config.Rules) > 0 {
		quotas := make([]quota, 0, len(p.config.Rules))
		for i, rule := range p.config.Rules {
			key, ok := resolveRuleKey(r, rule.Key)
			if !ok {
				continue
			}
			limit, err := resolveQuotaValue(r, rule.Count, fmt.Sprintf("rule %d count", i+1))
			if err != nil {
				continue
			}
			window, err := resolveQuotaValue(r, rule.TimeWindow, fmt.Sprintf("rule %d time_window", i+1))
			if err != nil {
				continue
			}
			windowDuration, err := quotaWindow(window, fmt.Sprintf("rule %d time_window", i+1))
			if err != nil {
				continue
			}
			headerPrefix := rule.HeaderPrefix
			if headerPrefix == "" {
				headerPrefix = strconv.Itoa(i + 1)
			}
			quotas = append(quotas, quota{
				key:          "rule:" + strconv.Itoa(i) + ":" + key,
				headerPrefix: headerPrefix,
				limit:        limit,
				window:       windowDuration,
			})
		}
		if len(quotas) == 0 {
			return nil, false, errNoUsableRules
		}
		return quotas, true, nil
	}

	q, ok, err := p.quotaForRequest(r)
	if err != nil || !ok {
		return nil, ok, err
	}
	if consumerName := fmt.Sprint(apisixctx.GetApisixVar(r, "$consumer_name")); consumerName != "" {
		q.key = "consumer:" + consumerName + ":" + q.key
	}
	return []quota{q}, true, nil
}

func resolveRuleKey(r *http.Request, key string) (string, bool) {
	resolved := 0
	key = variablePattern.ReplaceAllStringFunc(key, func(match string) string {
		variableName := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$"), "}")
		value := requestVariable(r, variableName)
		if value != "" {
			resolved++
		}
		return value
	})
	return key, resolved > 0 && key != ""
}

func (p *Plugin) quotaForRequest(r *http.Request) (quota, bool, error) {
	instanceName, hasInstance := PickedAIInstanceName(r)
	if hasInstance {
		for _, instance := range p.config.Instances {
			if instance.Name == instanceName {
				limit, err := resolveQuotaValue(r, instance.Limit, "instance "+instance.Name+" limit")
				if err != nil {
					return quota{}, false, err
				}
				window, err := resolveQuotaValue(r, instance.TimeWindow, "instance "+instance.Name+" time_window")
				if err != nil {
					return quota{}, false, err
				}
				windowDuration, err := quotaWindow(window, "instance "+instance.Name+" time_window")
				if err != nil {
					return quota{}, false, err
				}
				return quota{
					key:        "instance:" + instance.Name,
					headerName: instance.Name,
					limit:      limit,
					window:     windowDuration,
				}, true, nil
			}
		}
		if len(p.config.Instances) > 0 && (p.config.Limit == nil || p.config.TimeWindow == nil) {
			return quota{}, false, nil
		}
		limit, err := resolveQuotaValue(r, p.config.Limit, "limit")
		if err != nil {
			return quota{}, false, err
		}
		window, err := resolveQuotaValue(r, p.config.TimeWindow, "time_window")
		if err != nil {
			return quota{}, false, err
		}
		windowDuration, err := quotaWindow(window, "time_window")
		if err != nil {
			return quota{}, false, err
		}
		return quota{
			key:        "instance:" + instanceName,
			headerName: instanceName,
			limit:      limit,
			window:     windowDuration,
		}, true, nil
	}

	if len(p.config.Instances) > 0 {
		return quota{}, false, nil
	}
	limit, err := resolveQuotaValue(r, p.config.Limit, "limit")
	if err != nil {
		return quota{}, false, err
	}
	window, err := resolveQuotaValue(r, p.config.TimeWindow, "time_window")
	if err != nil {
		return quota{}, false, err
	}
	windowDuration, err := quotaWindow(window, "time_window")
	if err != nil {
		return quota{}, false, err
	}
	return quota{
		key:        "global",
		headerName: "global",
		limit:      limit,
		window:     windowDuration,
	}, true, nil
}

func quotaWindow(seconds int64, name string) (time.Duration, error) {
	const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
	if seconds > maxSeconds {
		return 0, fmt.Errorf("%s exceeds the maximum supported duration", name)
	}
	return time.Duration(seconds) * time.Second, nil
}

func staticQuotaValue(value any, name string) (int64, error) {
	if text, ok := value.(string); ok && strings.Contains(text, "$") {
		return 1, nil
	}
	return numericQuotaValue(value, name)
}

func resolveQuotaValue(r *http.Request, value any, name string) (int64, error) {
	if text, ok := value.(string); ok {
		value = quotaVariablePattern.ReplaceAllStringFunc(text, func(match string) string {
			parts := quotaVariablePattern.FindStringSubmatch(match)
			variableName := parts[1]
			if variableName == "" {
				variableName = parts[3]
			}
			if resolved := requestVariable(r, variableName); resolved != "" {
				return resolved
			}
			return strings.TrimSpace(parts[2])
		})
	}
	return numericQuotaValue(value, name)
}

func numericQuotaValue(value any, name string) (int64, error) {
	switch typed := value.(type) {
	case int:
		return positiveQuotaValue(int64(typed), name)
	case int64:
		return positiveQuotaValue(typed, name)
	case float64:
		if math.Trunc(typed) != typed {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		return positiveQuotaValue(int64(typed), name)
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must resolve to a positive integer: %w", name, err)
		}
		return positiveQuotaValue(parsed, name)
	default:
		return 0, fmt.Errorf("%s must be a positive integer or string", name)
	}
}

func positiveQuotaValue(value int64, name string) (int64, error) {
	if value <= 0 {
		return 0, fmt.Errorf("%s must be greater than 0", name)
	}
	return value, nil
}

func requestVariable(r *http.Request, key string) string {
	key = strings.TrimPrefix(key, "$")
	if after, ok := strings.CutPrefix(key, "http_"); ok {
		return r.Header.Get(strings.ReplaceAll(after, "_", "-"))
	}

	variableName := "$" + key
	if _, ok := v.RequestVars[variableName]; ok {
		return fmt.Sprint(v.GetRequestVar(r, variableName))
	}
	if _, ok := v.ApisixVars[variableName]; ok {
		return fmt.Sprint(v.GetApisixVar(r, variableName))
	}
	return v.GetNginxVar(r, variableName)
}

func (p *Plugin) reserve(ctx context.Context, q quota, tokens int64) bool {
	if tokens <= 0 || tokens > q.limit {
		return false
	}
	if p.redis != nil {
		allowed, err := p.redis.Eval(
			ctx,
			redisReserveScript,
			[]string{p.redisKey(q)},
			tokens,
			q.limit,
			q.window.Milliseconds(),
		).Int64()
		return err == nil && allowed == 1
	}

	accepted := false
	p.counters.Mutate(q.key, func(current counter, now time.Time) (counter, time.Duration, bool) {
		if current.reset.IsZero() || !now.Before(current.reset) {
			current = counter{reset: now.Add(q.window)}
		}
		if current.used > q.limit-tokens {
			return current, 0, false
		}
		current.used += tokens
		accepted = true
		return current, max(current.reset.Sub(now), 0), true
	})
	return accepted
}

func (p *Plugin) reconcile(ctx context.Context, q quota, delta int64, create bool) {
	delta = max(min(delta, q.limit), -q.limit)
	if p.redis != nil {
		_ = p.redis.Eval(
			ctx,
			redisReconcileScript,
			[]string{p.redisKey(q)},
			delta,
			q.limit,
			q.window.Milliseconds(),
			boolToInt(create),
		).Err()
		return
	}

	p.counters.Mutate(q.key, func(current counter, now time.Time) (counter, time.Duration, bool) {
		if current.reset.IsZero() || !now.Before(current.reset) {
			if !create {
				return current, 0, false
			}
			current = counter{reset: now.Add(q.window)}
		}
		current.used = max(min(current.used+delta, q.limit), 0)
		return current, max(current.reset.Sub(now), 0), true
	})
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (p *Plugin) responseTokenCost(body []byte) int64 {
	var decoded struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Usage == nil {
		return 0
	}

	if p.config.LimitStrategy == "expression" {
		return p.expressionCost(decoded.Usage)
	}

	value := ai_protocols.NumericUsage(decoded.Usage[p.config.LimitStrategy], true)
	if value < 0 {
		return 0
	}
	return value
}

func (p *Plugin) responseTokenCostForRequest(r *http.Request, body []byte) int64 {
	if p.config.LimitStrategy == "expression" {
		if rawUsage, ok := apisixctx.GetRequestVar(r, "$llm_raw_usage").(map[string]any); ok {
			return p.expressionCost(rawUsage)
		}
	}
	if usage, ok := apisixctx.GetRequestVar(r, "$ai_token_usage").(map[string]any); ok {
		if value := ai_protocols.NumericUsage(usage[p.config.LimitStrategy], true); value > 0 {
			return value
		}
	}
	return p.responseTokenCost(body)
}

func (p *Plugin) expressionCost(usage map[string]any) int64 {
	if p.costExpr == nil {
		return 0
	}
	value, err := p.costExpr.Eval(expressionParameters(usage))
	if err != nil {
		return 0
	}
	result, ok := value.(float64)
	if !ok || math.IsNaN(result) || math.IsInf(result, 0) || result >= float64(1<<63-1) {
		return 0
	}
	if result < 0 {
		return 0
	}
	return int64(math.Floor(result + 0.5))
}

type expressionParameters map[string]any

func (p expressionParameters) Get(name string) (any, error) {
	return numericExpressionValue(p[name]), nil
}

func numericExpressionValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

func costExpressionFunctions() map[string]govaluate.ExpressionFunction {
	return map[string]govaluate.ExpressionFunction{
		"abs": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			return math.Abs(values[0]), nil
		},
		"ceil": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			return math.Ceil(values[0]), nil
		},
		"floor": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			return math.Floor(values[0]), nil
		},
		"sqrt": unaryMathFunction(math.Sqrt),
		"exp":  unaryMathFunction(math.Exp),
		"log":  unaryMathFunction(math.Log),
		"sin":  unaryMathFunction(math.Sin),
		"cos":  unaryMathFunction(math.Cos),
		"tan":  unaryMathFunction(math.Tan),
		"asin": unaryMathFunction(math.Asin),
		"acos": unaryMathFunction(math.Acos),
		"atan": unaryMathFunction(math.Atan),
		"pow": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 2)
			if err != nil || len(values) != 2 {
				return nil, fmt.Errorf("pow expects exactly 2 numeric arguments")
			}
			return math.Pow(values[0], values[1]), nil
		},
		"max": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			result := values[0]
			for _, value := range values[1:] {
				if value > result {
					result = value
				}
			}
			return result, nil
		},
		"min": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			result := values[0]
			for _, value := range values[1:] {
				if value < result {
					result = value
				}
			}
			return result, nil
		},
	}
}

func unaryMathFunction(fn func(float64) float64) govaluate.ExpressionFunction {
	return func(arguments ...any) (any, error) {
		values, err := numericArguments(arguments, 1)
		if err != nil || len(values) != 1 {
			return nil, fmt.Errorf("expected exactly 1 numeric argument")
		}
		return fn(values[0]), nil
	}
}

func numericArguments(arguments []any, minimum int) ([]float64, error) {
	if len(arguments) < minimum {
		return nil, fmt.Errorf("expected at least %d numeric arguments", minimum)
	}

	values := make([]float64, len(arguments))
	for i, argument := range arguments {
		value, ok := argument.(float64)
		if !ok {
			return nil, fmt.Errorf("argument %d must be numeric", i+1)
		}
		values[i] = value
	}
	return values, nil
}

func (p *Plugin) writeQuotaHeaders(ctx context.Context, header http.Header, q quota) {
	if p.config.ShowLimitQuotaHeader != nil && !*p.config.ShowLimitQuotaHeader {
		return
	}

	used, reset := p.snapshot(ctx, q)
	remaining := max(q.limit-used, 0)
	if q.headerPrefix != "" {
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Limit", strconv.FormatInt(q.limit, 10))
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Reset", strconv.FormatInt(reset, 10))
		return
	}
	header.Set("X-AI-RateLimit-Limit-"+q.headerName, strconv.FormatInt(q.limit, 10))
	header.Set("X-AI-RateLimit-Remaining-"+q.headerName, strconv.FormatInt(remaining, 10))
	header.Set("X-AI-RateLimit-Reset-"+q.headerName, strconv.FormatInt(reset, 10))
}

func (p *Plugin) snapshot(ctx context.Context, q quota) (int64, int64) {
	if p.redis != nil {
		// One round trip: GET and PTTL in a single Lua script.
		values, err := p.redis.Eval(ctx, redisSnapshotScript, []string{p.redisKey(q)}).Slice()
		if err != nil || len(values) != 2 {
			return 0, max(int64(math.Ceil(q.window.Seconds())), 0)
		}
		used := snapshotInteger(values[0])
		ttl := snapshotInteger(values[1])
		if ttl < 0 {
			ttl = q.window.Milliseconds()
		}
		return used, max(int64(math.Ceil(float64(ttl)/1000)), 0)
	}
	used := int64(0)
	reset := max(int64(math.Ceil(q.window.Seconds())), 0)
	p.counters.Mutate(q.key, func(current counter, now time.Time) (counter, time.Duration, bool) {
		if current.reset.IsZero() || !now.Before(current.reset) {
			return current, 0, false
		}
		used = current.used
		reset = max(int64(math.Ceil(current.reset.Sub(now).Seconds())), 0)
		return current, 0, false
	})
	return used, reset
}

// snapshotInteger converts a Lua script reply element to an integer; nil
// replies (missing keys) and strings both decode like Redis integers.
func snapshotInteger(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case int64:
		return typed
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

func (p *Plugin) redisKey(q quota) string {
	scope := p.resourceScope
	if scope == "" {
		scope = "resource:0::0:"
	}
	return "apisix-go:ai-rate-limiting:" + scope + ":config:" + p.configIdentity + ":" + q.key
}

func (p *Plugin) reject(w http.ResponseWriter) {
	if p.config.RejectedMsg == "" {
		http.Error(w, http.StatusText(p.config.RejectedCode), p.config.RejectedCode)
		return
	}
	http.Error(w, p.config.RejectedMsg, p.config.RejectedCode)
}
