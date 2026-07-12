package graphql_limit_count

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu       sync.Mutex
	counters map[string]*counter
	now      func() time.Time

	redisLimiter countLimiter
	maxSize      int
	routeID      string
	metadata     Metadata
}

const (
	priority = 1004
	name     = "graphql-limit-count"
)

const schema = `
{
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
    "rules": {
      "type": "array",
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
          "key": {"type": "string"},
          "header_prefix": {"type": "string"}
        },
        "required": ["count", "time_window", "key"]
      }
    },
    "group": {
      "type": "string"
    },
    "key": {
      "type": "string",
      "default": "remote_addr"
    },
    "key_type": {
      "type": "string",
      "enum": ["var", "var_combination", "constant"],
      "default": "var"
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
    "policy": {
      "type": "string",
      "enum": ["local", "redis", "redis-cluster"],
      "default": "local"
    },
    "redis_host": {
      "type": "string",
      "minLength": 2
    },
    "redis_port": {
      "type": "integer",
      "minimum": 1,
      "default": 6379
    },
    "redis_username": {
      "type": "string",
      "minLength": 1
    },
    "redis_password": {
      "type": "string",
      "minLength": 0
    },
    "redis_database": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    },
    "redis_timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 1000
    },
    "redis_ssl": {
      "type": "boolean",
      "default": false
    },
    "redis_ssl_verify": {
      "type": "boolean",
      "default": false
    },
    "redis_keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 10000
    },
    "redis_keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 100
    },
    "redis_cluster_nodes": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "minLength": 2,
        "maxLength": 100
      }
    },
    "redis_cluster_name": {
      "type": "string"
    },
    "redis_cluster_ssl": {
      "type": "boolean",
      "default": false
    },
    "redis_cluster_ssl_verify": {
      "type": "boolean",
      "default": false
    },
    "allow_degradation": {
      "type": "boolean",
      "default": false
    },
    "show_limit_quota_header": {
      "type": "boolean",
      "default": true
    }
  },
  "allOf": [
    {
      "if": {
        "properties": {"policy": {"const": "redis"}},
        "required": ["policy"]
      },
      "then": {"required": ["redis_host"]}
    },
    {
      "if": {
        "properties": {"policy": {"const": "redis-cluster"}},
        "required": ["policy"]
      },
      "then": {"required": ["redis_cluster_nodes", "redis_cluster_name"]}
    }
  ],
  "oneOf": [
    {"required": ["count", "time_window"]},
    {"required": ["rules"]}
  ]
}
`

type Config struct {
	Count                 any      `json:"count,omitempty"`
	TimeWindow            any      `json:"time_window,omitempty"`
	Group                 string   `json:"group,omitempty"`
	Key                   string   `json:"key,omitempty"`
	KeyType               string   `json:"key_type,omitempty"`
	RejectedCode          int      `json:"rejected_code,omitempty"`
	RejectedMsg           string   `json:"rejected_msg,omitempty"`
	Policy                string   `json:"policy,omitempty"`
	RedisHost             string   `json:"redis_host,omitempty"`
	RedisPort             int      `json:"redis_port,omitempty"`
	RedisUsername         string   `json:"redis_username,omitempty"`
	RedisPassword         string   `json:"redis_password,omitempty"`
	RedisDatabase         int      `json:"redis_database,omitempty"`
	RedisTimeout          int      `json:"redis_timeout,omitempty"`
	RedisSSL              *bool    `json:"redis_ssl,omitempty"`
	RedisSSLVerify        *bool    `json:"redis_ssl_verify,omitempty"`
	RedisKeepaliveTimeout int      `json:"redis_keepalive_timeout,omitempty"`
	RedisKeepalivePool    int      `json:"redis_keepalive_pool,omitempty"`
	RedisClusterNodes     []string `json:"redis_cluster_nodes,omitempty"`
	RedisClusterName      string   `json:"redis_cluster_name,omitempty"`
	RedisClusterSSL       *bool    `json:"redis_cluster_ssl,omitempty"`
	RedisClusterSSLVerify *bool    `json:"redis_cluster_ssl_verify,omitempty"`
	AllowDegradation      *bool    `json:"allow_degradation,omitempty"`
	ShowLimitQuotaHeader  *bool    `json:"show_limit_quota_header,omitempty"`
	Rules                 []Rule   `json:"rules,omitempty"`
}

type Rule struct {
	Count        any    `json:"count"`
	TimeWindow   any    `json:"time_window"`
	Key          string `json:"key"`
	HeaderPrefix string `json:"header_prefix,omitempty"`
}

type Metadata struct {
	LimitHeader     string `json:"limit_header"`
	RemainingHeader string `json:"remaining_header"`
	ResetHeader     string `json:"reset_header"`
}

type counter struct {
	used    int64
	resetAt time.Time
}

var groupCounters = struct {
	sync.Mutex
	entries map[string]*counter
}{entries: map[string]*counter{}}

var graphqlLimitCountGroups = struct {
	sync.Mutex
	entries map[string]string
}{entries: map[string]string{}}

const redisLimitCountScript = `
local current = redis.call("INCRBY", KEYS[1], ARGV[1])
local ttl = redis.call("TTL", KEYS[1])
if ttl < 0 then
  redis.call("EXPIRE", KEYS[1], ARGV[3])
  ttl = tonumber(ARGV[3])
end

local limit = tonumber(ARGV[2])
local remaining = limit - current
if remaining < 0 then
  remaining = 0
end

local allowed = 1
if current > limit then
  allowed = 0
end

return {allowed, remaining, ttl}
`

type countLimiter interface {
	incoming(r *http.Request, key string, cost int64, count int64, timeWindow int64) (int64, int64, bool, error)
}

type graphqlRequest struct {
	Query string `json:"query"`
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
	if len(p.config.Rules) > 0 {
		if err := validateRules(p.config.Rules); err != nil {
			return err
		}
	} else {
		if err := validateStaticLimitValue(p.config.Count, "count"); err != nil {
			return err
		}
		if err := validateStaticLimitValue(p.config.TimeWindow, "time_window"); err != nil {
			return err
		}
	}
	if p.config.Key == "" {
		p.config.Key = "remote_addr"
	}
	if p.config.KeyType == "" {
		p.config.KeyType = "var"
	}
	if p.config.Policy == "" {
		p.config.Policy = "local"
	}
	switch p.config.Policy {
	case "local":
	case "redis":
		if p.config.RedisHost == "" {
			return fmt.Errorf("redis_host is required")
		}
		if p.config.RedisPort == 0 {
			p.config.RedisPort = 6379
		}
		if p.config.RedisTimeout == 0 {
			p.config.RedisTimeout = 1000
		}
		if p.config.RedisSSL == nil {
			value := false
			p.config.RedisSSL = &value
		}
		if p.config.RedisSSLVerify == nil {
			value := false
			p.config.RedisSSLVerify = &value
		}
		if p.config.RedisKeepalivePool == 0 {
			p.config.RedisKeepalivePool = 100
		}
		if p.config.RedisKeepaliveTimeout == 0 {
			p.config.RedisKeepaliveTimeout = 10000
		}
		if p.redisLimiter == nil {
			p.redisLimiter = p.newRedisLimiter()
		}
	case "redis-cluster":
		if len(p.config.RedisClusterNodes) == 0 {
			return fmt.Errorf("redis_cluster_nodes is required")
		}
		if p.config.RedisClusterName == "" {
			return fmt.Errorf("redis_cluster_name is required")
		}
		if p.config.RedisTimeout == 0 {
			p.config.RedisTimeout = 1000
		}
		if p.config.RedisClusterSSL == nil {
			value := false
			p.config.RedisClusterSSL = &value
		}
		if p.config.RedisClusterSSLVerify == nil {
			value := false
			p.config.RedisClusterSSLVerify = &value
		}
		if p.config.RedisKeepalivePool == 0 {
			p.config.RedisKeepalivePool = 100
		}
		if p.config.RedisKeepaliveTimeout == 0 {
			p.config.RedisKeepaliveTimeout = 10000
		}
		if p.redisLimiter == nil {
			p.redisLimiter = p.newRedisClusterLimiter()
		}
	default:
		return fmt.Errorf("not supported policy: %s", p.config.Policy)
	}
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = http.StatusServiceUnavailable
	}
	if p.config.AllowDegradation == nil {
		value := false
		p.config.AllowDegradation = &value
	}
	if p.config.ShowLimitQuotaHeader == nil {
		value := true
		p.config.ShowLimitQuotaHeader = &value
	}
	if err := p.registerGroup(); err != nil {
		return err
	}
	if p.counters == nil {
		p.counters = make(map[string]*counter)
	}
	if p.now == nil {
		p.now = time.Now
	}
	p.maxSize = 1048576
	if config.GlobalConfig != nil && config.GlobalConfig.GraphQL.MaxSize > 0 {
		p.maxSize = config.GlobalConfig.GraphQL.MaxSize
	}
	if p.metadata == (Metadata{}) {
		p.metadata = loadMetadata()
	}
	return nil
}

func (p *Plugin) SetResourceContext(route resource.Route, _ resource.Service) {
	p.routeID = route.ID
}

func (p *Plugin) counterNamespace() string {
	if p.config.Group != "" {
		return "group:" + p.config.Group
	}
	if p.routeID != "" {
		return "route:" + p.routeID
	}
	return "route:unknown"
}

func (p *Plugin) registerGroup() error {
	if p.config.Group == "" {
		return nil
	}
	fingerprint, err := json.Marshal(p.config)
	if err != nil {
		return fmt.Errorf("marshal graphql-limit-count group config: %w", err)
	}

	graphqlLimitCountGroups.Lock()
	defer graphqlLimitCountGroups.Unlock()
	current, ok := graphqlLimitCountGroups.entries[p.config.Group]
	if ok {
		if current != string(fingerprint) {
			return fmt.Errorf("group conf mismatched")
		}
		return nil
	}
	graphqlLimitCountGroups.entries[p.config.Group] = string(fingerprint)
	return nil
}

func validateRules(rules []Rule) error {
	seen := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if rule.Key == "" {
			return fmt.Errorf("graphql-limit-count rule key is required")
		}
		if _, ok := seen[rule.Key]; ok {
			return fmt.Errorf("duplicate key %q in rules", rule.Key)
		}
		seen[rule.Key] = struct{}{}
		if err := validateStaticLimitValue(rule.Count, "rule count"); err != nil {
			return err
		}
		if err := validateStaticLimitValue(rule.TimeWindow, "rule time_window"); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		query, ok := p.graphqlQuery(w, r)
		if !ok {
			return
		}

		depth, err := queryDepth(query)
		if err != nil {
			http.Error(w, "Invalid graphql request: failed to parse graphql query", http.StatusBadRequest)
			return
		}

		if len(p.config.Rules) > 0 {
			applied := 0
			for i, rule := range p.config.Rules {
				key, ok := resolveRuleKey(r, rule)
				if !ok {
					continue
				}
				count, err := resolveLimitValue(r, rule.Count, "rule count")
				if err != nil {
					continue
				}
				timeWindow, err := resolveLimitValue(r, rule.TimeWindow, "rule time_window")
				if err != nil {
					continue
				}
				applied++
				if !p.applyLimit(
					w,
					r,
					fmt.Sprintf("rule:%d:%s", i, key),
					int64(depth),
					count,
					timeWindow,
					ruleQuotaHeaders(rule, i),
				) {
					return
				}
			}
			if applied == 0 && !*p.config.AllowDegradation {
				http.Error(w, "failed to resolve graphql limit count rules", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		count, err := resolveLimitValue(r, p.config.Count, "count")
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "failed to resolve graphql limit count", http.StatusInternalServerError)
			return
		}
		timeWindow, err := resolveLimitValue(r, p.config.TimeWindow, "time_window")
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "failed to resolve graphql limit count", http.StatusInternalServerError)
			return
		}
		if !p.applyLimit(
			w,
			r,
			p.resolveKey(r),
			int64(depth),
			count,
			timeWindow,
			defaultQuotaHeaders(p.metadata),
		) {
			return
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

type quotaHeaders struct {
	limit     string
	remaining string
	reset     string
}

func defaultQuotaHeaders(metadata Metadata) quotaHeaders {
	if metadata.LimitHeader == "" {
		metadata.LimitHeader = "X-RateLimit-Limit"
	}
	if metadata.RemainingHeader == "" {
		metadata.RemainingHeader = "X-RateLimit-Remaining"
	}
	if metadata.ResetHeader == "" {
		metadata.ResetHeader = "X-RateLimit-Reset"
	}
	return quotaHeaders{
		limit:     metadata.LimitHeader,
		remaining: metadata.RemainingHeader,
		reset:     metadata.ResetHeader,
	}
}

func loadMetadata() (metadata Metadata) {
	defer func() {
		if recover() != nil {
			metadata = Metadata{}
		}
	}()
	if err := store.GetPluginMetadata("limit-count", &metadata); err != nil {
		return Metadata{}
	}
	return metadata
}

func ruleQuotaHeaders(rule Rule, index int) quotaHeaders {
	prefix := rule.HeaderPrefix
	if prefix == "" {
		prefix = strconv.Itoa(index + 1)
	}
	return quotaHeaders{
		limit:     "X-" + prefix + "-RateLimit-Limit",
		remaining: "X-" + prefix + "-RateLimit-Remaining",
		reset:     "X-" + prefix + "-RateLimit-Reset",
	}
}

func (p *Plugin) applyLimit(
	w http.ResponseWriter,
	r *http.Request,
	key string,
	cost int64,
	count int64,
	timeWindow int64,
	headers quotaHeaders,
) bool {
	remaining, reset, allowed, err := p.incoming(r, key, cost, count, timeWindow)
	if err != nil {
		if *p.config.AllowDegradation {
			return true
		}
		http.Error(w, "failed to limit graphql count", http.StatusInternalServerError)
		return false
	}
	if *p.config.ShowLimitQuotaHeader {
		w.Header().Set(headers.limit, strconv.FormatInt(count, 10))
		w.Header().Set(headers.remaining, strconv.FormatInt(remaining, 10))
		w.Header().Set(headers.reset, strconv.FormatInt(reset, 10))
	}
	if allowed {
		return true
	}

	rejectedMsg := "Limit exceeded"
	if p.config.RejectedMsg != "" {
		rejectedMsg = p.config.RejectedMsg
	}
	http.Error(w, rejectedMsg, p.config.RejectedCode)
	return false
}

func (p *Plugin) graphqlQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return "", false
	}

	body, err := readBody(r, p.maxSize)
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		http.Error(w, "Invalid graphql request: can't get graphql request body", http.StatusBadRequest)
		return "", false
	}

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var req graphqlRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid graphql request, "+err.Error(), http.StatusBadRequest)
			return "", false
		}
		if req.Query == "" {
			http.Error(w, "invalid graphql request, json body[query] is nil", http.StatusBadRequest)
			return "", false
		}
		return req.Query, true
	}

	if strings.HasPrefix(contentType, "application/graphql") {
		return string(body), true
	}

	http.Error(w, "invalid graphql request, error content-type: "+contentType, http.StatusBadRequest)
	return "", false
}

func (p *Plugin) incoming(
	r *http.Request,
	key string,
	cost int64,
	count int64,
	timeWindow int64,
) (int64, int64, bool, error) {
	if p.config.Policy == "redis" || p.config.Policy == "redis-cluster" {
		return p.redisLimiter.incoming(r, key, cost, count, timeWindow)
	}
	if p.config.Group != "" {
		groupCounters.Lock()
		defer groupCounters.Unlock()
		return incomingLocal(
			groupCounters.entries,
			p.counterNamespace()+":"+key,
			cost,
			count,
			timeWindow,
			p.now(),
		)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	return incomingLocal(p.counters, key, cost, count, timeWindow, p.now())
}

func incomingLocal(
	counters map[string]*counter,
	key string,
	cost int64,
	count int64,
	timeWindow int64,
	now time.Time,
) (int64, int64, bool, error) {
	counterKey := fmt.Sprintf("%d:%d:%s", count, timeWindow, key)
	c, ok := counters[counterKey]
	if !ok || !now.Before(c.resetAt) {
		c = &counter{resetAt: now.Add(time.Duration(timeWindow) * time.Second)}
		counters[counterKey] = c
	}

	reset := int64(c.resetAt.Sub(now).Seconds())
	if reset < 0 {
		reset = 0
	}

	if c.used+cost > count {
		return 0, reset, false, nil
	}
	c.used += cost
	return count - c.used, reset, true, nil
}

type redisCountLimiter struct {
	client    redis.UniversalClient
	namespace string
}

func (p *Plugin) newRedisLimiter() countLimiter {
	configUID := shared.NewConfigUID()
	configUID.Add(
		p.config.RedisHost,
		p.config.RedisPort,
		p.config.RedisUsername,
		p.config.RedisPassword,
		p.config.RedisDatabase,
		p.config.RedisTimeout,
		*p.config.RedisSSL,
		*p.config.RedisSSLVerify,
		p.config.RedisKeepaliveTimeout,
		p.config.RedisKeepalivePool,
	)

	options := &redis.Options{
		Addr:         fmt.Sprintf("%s:%d", p.config.RedisHost, p.config.RedisPort),
		Username:     p.config.RedisUsername,
		Password:     p.config.RedisPassword,
		DB:           p.config.RedisDatabase,
		DialTimeout:  time.Duration(p.config.RedisTimeout) * time.Millisecond,
		ReadTimeout:  time.Duration(p.config.RedisTimeout) * time.Millisecond,
		WriteTimeout: time.Duration(p.config.RedisTimeout) * time.Millisecond,
		PoolSize:     p.config.RedisKeepalivePool,
	}
	if p.config.RedisKeepaliveTimeout > 0 {
		options.ConnMaxIdleTime = time.Duration(p.config.RedisKeepaliveTimeout) * time.Millisecond
	}
	if p.config.RedisSSL != nil && *p.config.RedisSSL {
		options.TLSConfig = &tls.Config{InsecureSkipVerify: !*p.config.RedisSSLVerify}
	}

	client := shared.LoadOrStoreClient(name, configUID, redis.NewClient(options)).(redis.UniversalClient)
	return &redisCountLimiter{client: client, namespace: p.counterNamespace()}
}

func (p *Plugin) newRedisClusterLimiter() countLimiter {
	configUID := shared.NewConfigUID()
	configUID.Add(
		p.config.RedisClusterName,
		strings.Join(p.config.RedisClusterNodes, ","),
		p.config.RedisPassword,
		p.config.RedisTimeout,
		*p.config.RedisClusterSSL,
		*p.config.RedisClusterSSLVerify,
		p.config.RedisKeepaliveTimeout,
		p.config.RedisKeepalivePool,
	)

	options := &redis.ClusterOptions{
		Addrs:        append([]string(nil), p.config.RedisClusterNodes...),
		Password:     p.config.RedisPassword,
		DialTimeout:  time.Duration(p.config.RedisTimeout) * time.Millisecond,
		ReadTimeout:  time.Duration(p.config.RedisTimeout) * time.Millisecond,
		WriteTimeout: time.Duration(p.config.RedisTimeout) * time.Millisecond,
		PoolSize:     p.config.RedisKeepalivePool,
	}
	if p.config.RedisKeepaliveTimeout > 0 {
		options.ConnMaxIdleTime = time.Duration(p.config.RedisKeepaliveTimeout) * time.Millisecond
	}
	if *p.config.RedisClusterSSL {
		options.TLSConfig = &tls.Config{InsecureSkipVerify: !*p.config.RedisClusterSSLVerify}
	}

	client := shared.LoadOrStoreClient(name, configUID, redis.NewClusterClient(options)).(redis.UniversalClient)
	return &redisCountLimiter{client: client, namespace: p.counterNamespace()}
}

func (l *redisCountLimiter) incoming(
	r *http.Request,
	key string,
	cost int64,
	count int64,
	timeWindow int64,
) (int64, int64, bool, error) {
	result, err := l.client.Eval(
		r.Context(),
		redisLimitCountScript,
		[]string{"plugin-graphql-limit-count:" + l.namespace + ":" + key},
		cost,
		count,
		timeWindow,
	).Result()
	if err != nil {
		return 0, 0, false, err
	}

	values, ok := result.([]any)
	if !ok || len(values) != 3 {
		return 0, 0, false, fmt.Errorf("unexpected redis graphql-limit-count result: %v", result)
	}
	allowed, ok := redisInt(values[0])
	if !ok {
		return 0, 0, false, fmt.Errorf("unexpected redis graphql-limit-count allowed value: %v", values[0])
	}
	remaining, ok := redisInt(values[1])
	if !ok {
		return 0, 0, false, fmt.Errorf("unexpected redis graphql-limit-count remaining value: %v", values[1])
	}
	reset, ok := redisInt(values[2])
	if !ok {
		return 0, 0, false, fmt.Errorf("unexpected redis graphql-limit-count reset value: %v", values[2])
	}

	return remaining, reset, allowed == 1, nil
}

func redisInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		return int64(v), true
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func validateStaticLimitValue(value any, name string) error {
	if value == nil {
		return fmt.Errorf("%s is required", name)
	}
	if expression, ok := value.(string); ok {
		if strings.Contains(expression, "$") {
			return nil
		}
		parsed, err := strconv.ParseInt(expression, 10, 64)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("%s must be a positive integer", name)
		}
		return nil
	}
	_, err := numericLimitValue(value, name)
	return err
}

func resolveLimitValue(r *http.Request, value any, name string) (int64, error) {
	if expression, ok := value.(string); ok {
		for _, variableName := range templateVariables(expression) {
			resolved := requestVar(r, variableName)
			expression = strings.ReplaceAll(expression, "${"+variableName+"}", resolved)
			expression = strings.ReplaceAll(expression, "$"+variableName, resolved)
		}
		parsed, err := strconv.ParseInt(expression, 10, 64)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%s must resolve to a positive integer", name)
		}
		return parsed, nil
	}
	return numericLimitValue(value, name)
}

func numericLimitValue(value any, name string) (int64, error) {
	switch typed := value.(type) {
	case int:
		if typed > 0 {
			return int64(typed), nil
		}
	case int64:
		if typed > 0 {
			return typed, nil
		}
	case float64:
		if typed > 0 && math.Trunc(typed) == typed {
			return int64(typed), nil
		}
	}
	return 0, fmt.Errorf("%s must be a positive integer", name)
}

func resolveRuleKey(r *http.Request, rule Rule) (string, bool) {
	key := rule.Key
	resolved := 0
	for _, variableName := range templateVariables(key) {
		value := requestVar(r, variableName)
		if value != "" {
			resolved++
		}
		key = strings.ReplaceAll(key, "${"+variableName+"}", value)
		key = strings.ReplaceAll(key, "$"+variableName, value)
	}
	return key, resolved > 0 && key != ""
}

func (p *Plugin) resolveKey(r *http.Request) string {
	switch p.config.KeyType {
	case "constant":
		return p.config.Key
	case "var_combination":
		key := p.config.Key
		resolved := 0
		for _, name := range templateVariables(key) {
			value := requestVar(r, name)
			if value != "" {
				resolved++
			}
			key = strings.ReplaceAll(key, "${"+name+"}", value)
			key = strings.ReplaceAll(key, "$"+name, value)
		}
		if resolved > 0 {
			return key
		}
	}

	if value := requestVar(r, p.config.Key); value != "" {
		return value
	}
	return requestVar(r, "remote_addr")
}

func readBody(r *http.Request, maxSize int) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxSize)+1))
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err == nil && len(body) > maxSize {
		err = fmt.Errorf("graphql request body exceeds maximum size %d", maxSize)
	}
	return body, err
}

func requestVar(r *http.Request, key string) string {
	key = strings.TrimPrefix(key, "$")
	if strings.HasPrefix(key, "http_") {
		header := strings.ReplaceAll(strings.TrimPrefix(key, "http_"), "_", "-")
		return r.Header.Get(header)
	}

	if key == "remote_addr" && r.RemoteAddr != "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			return host
		}
		return r.RemoteAddr
	}

	value := variable.GetNginxVar(r, "$"+key)
	if key == "remote_addr" {
		if host, _, err := net.SplitHostPort(value); err == nil {
			return host
		}
	}
	return value
}

func queryDepth(query string) (int, error) {
	tokens := tokenize(query)
	parser := graphQLParser{tokens: tokens}
	doc, err := parser.parseDocument()
	if err != nil {
		return 0, err
	}
	if err := doc.validate(); err != nil {
		return 0, err
	}
	return doc.depth(), nil
}

func templateVariables(template string) []string {
	var variables []string
	for i := 0; i < len(template); i++ {
		if template[i] != '$' {
			continue
		}
		start := i + 1
		end := start
		if start < len(template) && template[start] == '{' {
			start++
			end = start
			for end < len(template) && template[end] != '}' {
				end++
			}
			if end < len(template) {
				variables = append(variables, template[start:end])
				i = end
			}
			continue
		}
		for end < len(template) && isNameChar(template[end]) {
			end++
		}
		if end > start {
			variables = append(variables, template[start:end])
			i = end - 1
		}
	}
	return variables
}

type graphQLDocument struct {
	operations []selectionSet
	fragments  map[string]selectionSet
}

func (d graphQLDocument) validate() error {
	for _, operation := range d.operations {
		if err := validateSelectionSet(operation, d.fragments, map[string]bool{}); err != nil {
			return err
		}
	}
	return nil
}

func validateSelectionSet(
	selections selectionSet,
	fragments map[string]selectionSet,
	stack map[string]bool,
) error {
	for _, item := range selections {
		if item.fragment != "" {
			if stack[item.fragment] {
				return fmt.Errorf("cyclic graphql fragment %q", item.fragment)
			}
			fragment, ok := fragments[item.fragment]
			if !ok {
				return fmt.Errorf("undefined graphql fragment %q", item.fragment)
			}
			stack[item.fragment] = true
			if err := validateSelectionSet(fragment, fragments, stack); err != nil {
				return err
			}
			delete(stack, item.fragment)
		}
		if item.inline {
			if err := validateSelectionSet(item.child, fragments, stack); err != nil {
				return err
			}
		}
		if err := validateSelectionSet(item.child, fragments, stack); err != nil {
			return err
		}
	}
	return nil
}

func (d graphQLDocument) depth() int {
	depth := 0
	for _, op := range d.operations {
		depth = max(depth, op.depth(d.fragments, map[string]bool{}))
	}
	return max(depth, 1)
}

type selectionSet []selection

func (s selectionSet) depth(fragments map[string]selectionSet, visited map[string]bool) int {
	depth := 0
	for _, item := range s {
		depth = max(depth, item.depth(fragments, visited))
	}
	return depth
}

type selection struct {
	name     string
	child    selectionSet
	fragment string
	inline   bool
}

func (s selection) depth(fragments map[string]selectionSet, visited map[string]bool) int {
	if s.fragment != "" {
		if visited[s.fragment] {
			return 0
		}
		fragment, ok := fragments[s.fragment]
		if !ok {
			return 0
		}
		visited[s.fragment] = true
		depth := fragment.depth(fragments, visited)
		delete(visited, s.fragment)
		return depth
	}
	if s.inline {
		return s.child.depth(fragments, visited)
	}
	if len(s.child) == 0 {
		return 1
	}
	return 1 + s.child.depth(fragments, visited)
}

type graphQLParser struct {
	tokens []string
	pos    int
}

func (p *graphQLParser) parseDocument() (graphQLDocument, error) {
	doc := graphQLDocument{fragments: map[string]selectionSet{}}
	for p.hasNext() {
		if p.peek() == "fragment" {
			name, set, err := p.parseFragment()
			if err != nil {
				return doc, err
			}
			doc.fragments[name] = set
			continue
		}

		set, err := p.parseOperation()
		if err != nil {
			return doc, err
		}
		doc.operations = append(doc.operations, set)
	}
	if len(doc.operations) == 0 {
		return doc, fmt.Errorf("empty graphql query")
	}
	return doc, nil
}

func (p *graphQLParser) parseFragment() (string, selectionSet, error) {
	p.next()
	if !p.hasNext() {
		return "", nil, fmt.Errorf("missing fragment name")
	}
	name := p.next()
	set, err := p.skipToSelectionSet()
	return name, set, err
}

func (p *graphQLParser) parseOperation() (selectionSet, error) {
	if p.peek() == "{" {
		return p.parseSelectionSet()
	}
	return p.skipToSelectionSet()
}

func (p *graphQLParser) skipToSelectionSet() (selectionSet, error) {
	for p.hasNext() && p.peek() != "{" {
		p.next()
	}
	if !p.hasNext() {
		return nil, fmt.Errorf("missing selection set")
	}
	return p.parseSelectionSet()
}

func (p *graphQLParser) parseSelectionSet() (selectionSet, error) {
	if !p.consume("{") {
		return nil, fmt.Errorf("missing opening selection")
	}

	var selections selectionSet
	for p.hasNext() && p.peek() != "}" {
		if p.peek() == "..." {
			p.next()
			if !p.hasNext() {
				return nil, fmt.Errorf("missing fragment spread")
			}
			if p.peek() == "on" {
				p.next()
				if p.hasNext() {
					p.next()
				}
				child, err := p.skipToSelectionSet()
				if err != nil {
					return nil, err
				}
				selections = append(selections, selection{inline: true, child: child})
				continue
			}
			selections = append(selections, selection{fragment: p.next()})
			continue
		}

		field := selection{name: p.next()}
		p.skipArgumentsAndDirectives()
		if p.hasNext() && p.peek() == "{" {
			child, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			field.child = child
		}
		selections = append(selections, field)
	}
	if !p.consume("}") {
		return nil, fmt.Errorf("missing closing selection")
	}
	return selections, nil
}

func (p *graphQLParser) skipArgumentsAndDirectives() {
	depth := 0
	for p.hasNext() {
		tok := p.peek()
		switch tok {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		case "{", "}":
			if depth == 0 {
				return
			}
		}
		p.next()
	}
}

func (p *graphQLParser) consume(token string) bool {
	if !p.hasNext() || p.peek() != token {
		return false
	}
	p.next()
	return true
}

func (p *graphQLParser) peek() string {
	return p.tokens[p.pos]
}

func (p *graphQLParser) next() string {
	token := p.tokens[p.pos]
	p.pos++
	return token
}

func (p *graphQLParser) hasNext() bool {
	return p.pos < len(p.tokens)
}

func tokenize(query string) []string {
	var tokens []string
	for i := 0; i < len(query); {
		switch ch := query[i]; {
		case ch == '#':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case ch == '"':
			i = skipString(query, i)
		case strings.HasPrefix(query[i:], "..."):
			tokens = append(tokens, "...")
			i += 3
		case strings.ContainsRune("{}()", rune(ch)):
			tokens = append(tokens, string(ch))
			i++
		case isNameChar(ch):
			start := i
			for i < len(query) && isNameChar(query[i]) {
				i++
			}
			tokens = append(tokens, query[start:i])
		default:
			i++
		}
	}
	return tokens
}

func skipString(query string, start int) int {
	i := start + 1
	for i < len(query) {
		if query[i] == '\\' {
			i += 2
			continue
		}
		if query[i] == '"' {
			return i + 1
		}
		i++
	}
	return i
}

func isNameChar(ch byte) bool {
	return ch == '_' || ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
}
