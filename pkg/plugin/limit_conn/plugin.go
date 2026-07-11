package limit_conn

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu        sync.Mutex
	conns     map[string]int
	unitDelay float64

	redisLimiter connLimiter
	routeID      string
}

const (
	priority = 1003
	name     = "limit-conn"
)

const schema = `
{
  "type": "object",
  "properties": {
    "conn": {
      "oneOf": [
        {
          "type": "integer",
          "exclusiveMinimum": 0
        },
        {
          "type": "string",
          "minLength": 1
        }
      ]
    },
    "burst": {
      "oneOf": [
        {
          "type": "integer",
          "minimum": 0
        },
        {
          "type": "string",
          "minLength": 1
        }
      ]
    },
    "default_conn_delay": {
      "type": "number",
      "exclusiveMinimum": 0
    },
    "rules": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "conn": {
            "oneOf": [
              {
                "type": "integer",
                "exclusiveMinimum": 0
              },
              {
                "type": "string",
                "minLength": 1
              }
            ]
          },
          "burst": {
            "oneOf": [
              {
                "type": "integer",
                "minimum": 0
              },
              {
                "type": "string",
                "minLength": 1
              }
            ]
          },
          "key": {
            "type": "string"
          }
        },
        "required": ["conn", "burst", "key"]
      }
    },
    "key": {
      "type": "string",
      "minLength": 1
    },
    "key_type": {
      "type": "string",
      "enum": ["var", "var_combination"],
      "default": "var"
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
    "key_ttl": {
      "type": "integer",
      "default": 3600
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
    "allow_degradation": {
      "type": "boolean",
      "default": false
    },
    "only_use_default_delay": {
      "type": "boolean",
      "default": false
    }
  },
  "oneOf": [
    {"required": ["conn", "burst", "default_conn_delay", "key"]},
    {"required": ["default_conn_delay", "rules"]}
  ],
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
  ]
}
`

type Config struct {
	Conn                  any      `json:"conn"`
	Burst                 any      `json:"burst"`
	DefaultConnDelay      float64  `json:"default_conn_delay"`
	Key                   string   `json:"key"`
	KeyType               string   `json:"key_type,omitempty"`
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
	RedisKeyTTL           int      `json:"key_ttl,omitempty"`
	RejectedCode          int      `json:"rejected_code,omitempty"`
	RejectedMsg           string   `json:"rejected_msg,omitempty"`
	AllowDegradation      *bool    `json:"allow_degradation,omitempty"`
	OnlyUseDefaultDelay   bool     `json:"only_use_default_delay,omitempty"`
	Rules                 []Rule   `json:"rules,omitempty"`

	rejectBody string
}

type Rule struct {
	Conn  any    `json:"conn"`
	Burst any    `json:"burst"`
	Key   string `json:"key"`
}

type admission struct {
	key string
}

type connLimiter interface {
	incoming(key string, conn int, burst int) (time.Duration, bool, error)
	leaving(key string, latency *time.Duration) error
}

var varPattern = regexp.MustCompile(`\$\{([0-9A-Za-z_]+)\}|\$([0-9A-Za-z_]+)`)

const redisLimitConnIncomingScript = `
local current = redis.call("INCR", KEYS[1])
redis.call("PEXPIRE", KEYS[1], ARGV[4])

local conn = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local default_delay = tonumber(ARGV[3])
local limit = conn + burst

if current > limit then
  local after_decr = redis.call("DECR", KEYS[1])
  if after_decr <= 0 then
    redis.call("DEL", KEYS[1])
  end
  return {0, 0}
end

local delay = 0
if current > conn then
  local multiplier = math.floor((current - 1) / conn)
  delay = multiplier * default_delay
end

return {1, math.floor(delay * 1000)}
`

const redisLimitConnLeavingScript = `
local current = redis.call("DECR", KEYS[1])
if current <= 0 then
  redis.call("DEL", KEYS[1])
end
return current
`

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.DefaultConnDelay <= 0 {
		return fmt.Errorf("default_conn_delay must be greater than 0")
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
			b := false
			p.config.RedisSSL = &b
		}
		if p.config.RedisSSLVerify == nil {
			b := false
			p.config.RedisSSLVerify = &b
		}
		if p.config.RedisKeepaliveTimeout == 0 {
			p.config.RedisKeepaliveTimeout = 10000
		}
		if p.config.RedisKeepalivePool == 0 {
			p.config.RedisKeepalivePool = 100
		}
		if p.config.RedisKeyTTL == 0 {
			p.config.RedisKeyTTL = 3600
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
		if p.config.RedisKeepaliveTimeout == 0 {
			p.config.RedisKeepaliveTimeout = 10000
		}
		if p.config.RedisKeepalivePool == 0 {
			p.config.RedisKeepalivePool = 100
		}
		if p.config.RedisKeyTTL == 0 {
			p.config.RedisKeyTTL = 3600
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
		b := false
		p.config.AllowDegradation = &b
	}

	if p.config.RejectedMsg != "" {
		body, _ := json.Marshal(map[string]string{"error_msg": p.config.RejectedMsg})
		p.config.rejectBody = util.BytesToString(body)
	}

	if p.conns == nil {
		p.conns = make(map[string]int)
	}
	p.unitDelay = p.config.DefaultConnDelay

	if len(p.config.Rules) > 0 {
		return validateRules(p.config.Rules)
	}

	if _, _, err := staticLimitValue(p.config.Conn, "conn", false); err != nil {
		return err
	}
	if _, _, err := staticLimitValue(p.config.Burst, "burst", true); err != nil {
		return err
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) SetResourceContext(route resource.Route, _ resource.Service) {
	p.routeID = route.ID
}

func (p *Plugin) scopedKey(key string) string {
	if p.routeID == "" {
		return key
	}
	return "route:" + p.routeID + ":" + key
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if len(p.config.Rules) > 0 {
			admissions, delay, allowed, err := p.increaseRules(r)
			if err != nil {
				if *p.config.AllowDegradation {
					next.ServeHTTP(w, r)
					return
				}
				http.Error(w, "failed to limit conn", http.StatusInternalServerError)
				return
			}
			if !allowed {
				p.reject(w)
				return
			}
			if len(admissions) == 0 {
				if *p.config.AllowDegradation {
					next.ServeHTTP(w, r)
					return
				}
				http.Error(w, "failed to get limit conn rules", http.StatusInternalServerError)
				return
			}
			if delay > 0 {
				time.Sleep(delay)
			}

			started := time.Now()
			defer func() {
				latency := time.Since(started)
				p.decreaseAdmissions(admissions, &latency)
			}()
			next.ServeHTTP(w, r)
			return
		}

		key := p.resolveKey(r)
		conn, burst, err := p.resolveLimits(r)
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "failed to resolve limit conn config", http.StatusInternalServerError)
			return
		}
		delay, allowed, err := p.increase(key, conn, burst)
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "failed to limit conn", http.StatusInternalServerError)
			return
		}
		if !allowed {
			p.reject(w)
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}

		started := time.Now()
		defer func() {
			latency := time.Since(started)
			p.decrease(key, &latency)
		}()
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func validateRules(rules []Rule) error {
	for _, rule := range rules {
		if _, _, err := staticLimitValue(rule.Conn, "rule conn", false); err != nil {
			return err
		}
		if _, _, err := staticLimitValue(rule.Burst, "rule burst", true); err != nil {
			return err
		}
		if rule.Key == "" {
			return fmt.Errorf("limit-conn rule key is required")
		}
	}
	return nil
}

func (p *Plugin) resolveLimits(r *http.Request) (int, int, error) {
	conn, err := resolveLimitValue(r, p.config.Conn, "conn", false)
	if err != nil {
		return 0, 0, err
	}
	burst, err := resolveLimitValue(r, p.config.Burst, "burst", true)
	if err != nil {
		return 0, 0, err
	}
	return conn, burst, nil
}

func (p *Plugin) resolveRuleLimits(r *http.Request, rule Rule) (int, int, bool) {
	conn, err := resolveLimitValue(r, rule.Conn, "rule conn", false)
	if err != nil {
		return 0, 0, false
	}
	burst, err := resolveLimitValue(r, rule.Burst, "rule burst", true)
	if err != nil {
		return 0, 0, false
	}
	return conn, burst, true
}

func staticLimitValue(value any, name string, allowZero bool) (int, bool, error) {
	if value == nil {
		return 0, false, fmt.Errorf("%s is required", name)
	}

	if expr, ok := value.(string); ok {
		if strings.Contains(expr, "$") {
			return 0, false, nil
		}
		parsed, err := parseLimitInt(expr, name, allowZero)
		if err != nil {
			return 0, false, err
		}
		return parsed, true, nil
	}

	parsed, err := numericLimitValue(value, name, allowZero)
	if err != nil {
		return 0, false, err
	}
	return parsed, true, nil
}

func resolveLimitValue(r *http.Request, value any, name string, allowZero bool) (int, error) {
	if expr, ok := value.(string); ok {
		resolved := varPattern.ReplaceAllStringFunc(expr, func(match string) string {
			varName := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
			varName = strings.TrimSuffix(varName, "}")
			return requestVar(r, varName)
		})
		return parseLimitInt(resolved, name, allowZero)
	}

	return numericLimitValue(value, name, allowZero)
}

func numericLimitValue(value any, name string, allowZero bool) (int, error) {
	switch v := value.(type) {
	case int:
		if err := validateLimitInt(v, name, allowZero); err != nil {
			return 0, err
		}
		return v, nil
	case int64:
		maxInt := int64(int(^uint(0) >> 1))
		minInt := -maxInt - 1
		if v < minInt || v > maxInt {
			return 0, fmt.Errorf("%s exceeds int range", name)
		}
		parsed := int(v)
		if err := validateLimitInt(parsed, name, allowZero); err != nil {
			return 0, err
		}
		return parsed, nil
	case float64:
		if math.Trunc(v) != v {
			return 0, fmt.Errorf("%s must resolve to an integer", name)
		}
		maxInt := float64(int(^uint(0) >> 1))
		minInt := -maxInt - 1
		if v < minInt || v > maxInt {
			return 0, fmt.Errorf("%s exceeds int range", name)
		}
		parsed := int(v)
		if err := validateLimitInt(parsed, name, allowZero); err != nil {
			return 0, err
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%s must be an integer or string expression", name)
	}
}

func parseLimitInt(value string, name string, allowZero bool) (int, error) {
	parsed, err := strconv.ParseInt(value, 10, 0)
	if err != nil {
		return 0, fmt.Errorf("%s must resolve to an integer: %w", name, err)
	}
	result := int(parsed)
	if err := validateLimitInt(result, name, allowZero); err != nil {
		return 0, err
	}
	return result, nil
}

func validateLimitInt(value int, name string, allowZero bool) error {
	if allowZero {
		if value < 0 {
			return fmt.Errorf("%s must be greater than or equal to 0", name)
		}
		return nil
	}
	if value <= 0 {
		return fmt.Errorf("%s must be greater than 0", name)
	}
	return nil
}

func (p *Plugin) reject(w http.ResponseWriter) {
	if p.config.RejectedMsg != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(p.config.RejectedCode)
		_, _ = w.Write([]byte(p.config.rejectBody))
		return
	}
	w.WriteHeader(p.config.RejectedCode)
}

func (p *Plugin) increaseRules(r *http.Request) ([]admission, time.Duration, bool, error) {
	var admissions []admission
	var delay time.Duration
	for i, rule := range p.config.Rules {
		key, ok := p.resolveRuleKey(r, i, rule)
		if !ok {
			continue
		}

		conn, burst, ok := p.resolveRuleLimits(r, rule)
		if !ok {
			continue
		}

		nextDelay, allowed, err := p.increase(key, conn, burst)
		if err != nil {
			p.decreaseAdmissions(admissions, nil)
			return nil, 0, false, err
		}
		if !allowed {
			p.decreaseAdmissions(admissions, nil)
			return nil, 0, false, nil
		}
		admissions = append(admissions, admission{key: key})
		delay += nextDelay
	}

	return admissions, delay, true, nil
}

func (p *Plugin) decreaseAdmissions(admissions []admission, latency *time.Duration) {
	for _, admission := range admissions {
		p.decrease(admission.key, latency)
	}
}

func (p *Plugin) increase(key string, conn int, burst int) (time.Duration, bool, error) {
	key = p.scopedKey(key)
	if p.config.Policy == "redis" || p.config.Policy == "redis-cluster" {
		return p.redisLimiter.incoming(key, conn, burst)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	current := p.conns[key] + 1
	limit := conn + burst
	if current > limit {
		return 0, false, nil
	}

	p.conns[key] = current
	if current > conn {
		return connectionDelay(current, conn, p.unitDelay), true, nil
	}

	return 0, true, nil
}

func connectionDelay(current int, conn int, unitDelay float64) time.Duration {
	multiplier := (current - 1) / conn
	return time.Duration(float64(multiplier) * unitDelay * float64(time.Second))
}

func (p *Plugin) decrease(key string, latency *time.Duration) {
	key = p.scopedKey(key)
	if p.config.Policy == "redis" || p.config.Policy == "redis-cluster" {
		_ = p.redisLimiter.leaving(key, latency)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if latency != nil && !p.config.OnlyUseDefaultDelay {
		p.unitDelay = (p.unitDelay + latency.Seconds()) / 2
	}

	current := p.conns[key]
	if current <= 1 {
		delete(p.conns, key)
		return
	}
	p.conns[key] = current - 1
}

type redisConnLimiter struct {
	mu                  sync.Mutex
	client              redis.UniversalClient
	unitDelay           float64
	keyTTL              time.Duration
	onlyUseDefaultDelay bool
}

func (p *Plugin) newRedisLimiter() connLimiter {
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
	client := shared.LoadOrStoreClient(name, configUID, redis.NewClient(p.redisOptions())).(redis.UniversalClient)
	return p.newRedisConnLimiter(client)
}

func (p *Plugin) redisOptions() *redis.Options {
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
	return options
}

func (p *Plugin) newRedisClusterLimiter() connLimiter {
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
	client := shared.LoadOrStoreClient(
		name,
		configUID,
		redis.NewClusterClient(p.redisClusterOptions()),
	).(redis.UniversalClient)
	return p.newRedisConnLimiter(client)
}

func (p *Plugin) newRedisConnLimiter(client redis.UniversalClient) connLimiter {
	return &redisConnLimiter{
		client:              client,
		unitDelay:           p.config.DefaultConnDelay,
		keyTTL:              time.Duration(p.config.RedisKeyTTL) * time.Second,
		onlyUseDefaultDelay: p.config.OnlyUseDefaultDelay,
	}
}

func (p *Plugin) redisClusterOptions() *redis.ClusterOptions {
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
	if p.config.RedisClusterSSL != nil && *p.config.RedisClusterSSL {
		options.TLSConfig = &tls.Config{InsecureSkipVerify: !*p.config.RedisClusterSSLVerify}
	}
	return options
}

func (l *redisConnLimiter) incoming(key string, conn int, burst int) (time.Duration, bool, error) {
	l.mu.Lock()
	unitDelay := l.unitDelay
	l.mu.Unlock()
	result, err := l.client.Eval(
		context.Background(),
		redisLimitConnIncomingScript,
		[]string{"plugin-limit-conn:" + key},
		conn,
		burst,
		unitDelay,
		l.keyTTL.Milliseconds(),
	).Result()
	if err != nil {
		return 0, false, err
	}

	values, ok := result.([]any)
	if !ok || len(values) != 2 {
		return 0, false, fmt.Errorf("unexpected redis limit-conn result: %v", result)
	}
	allowed, ok := redisInt(values[0])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-conn allowed value: %v", values[0])
	}
	delayMs, ok := redisInt(values[1])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-conn delay value: %v", values[1])
	}

	return time.Duration(delayMs) * time.Millisecond, allowed == 1, nil
}

func (l *redisConnLimiter) leaving(key string, latency *time.Duration) error {
	err := l.client.Eval(
		context.Background(),
		redisLimitConnLeavingScript,
		[]string{"plugin-limit-conn:" + key},
	).Err()
	if err != nil || latency == nil || l.onlyUseDefaultDelay {
		return err
	}
	l.mu.Lock()
	l.unitDelay = (l.unitDelay + latency.Seconds()) / 2
	l.mu.Unlock()
	return nil
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

func (p *Plugin) resolveKey(r *http.Request) string {
	var key string
	if p.config.KeyType == "var_combination" {
		resolved := 0
		key = varPattern.ReplaceAllStringFunc(p.config.Key, func(match string) string {
			name := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
			name = strings.TrimSuffix(name, "}")
			value := requestVar(r, name)
			if value != "" {
				resolved++
			}
			return value
		})
		if resolved == 0 {
			key = ""
		}
	} else {
		key = requestVar(r, p.config.Key)
	}

	if key == "" {
		key = requestVar(r, "remote_addr")
	}
	return key
}

func (p *Plugin) resolveRuleKey(r *http.Request, index int, rule Rule) (string, bool) {
	resolved := 0
	key := varPattern.ReplaceAllStringFunc(rule.Key, func(match string) string {
		name := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
		name = strings.TrimSuffix(name, "}")
		resolved++
		return requestVar(r, name)
	})
	if resolved == 0 {
		return "", false
	}

	return fmt.Sprintf("rule:%d:%s", index, key), true
}

func requestVar(r *http.Request, key string) string {
	key = strings.TrimPrefix(key, "$")

	if strings.HasPrefix(key, "http_") {
		header := strings.ReplaceAll(strings.TrimPrefix(key, "http_"), "_", "-")
		return r.Header.Get(header)
	}

	value := v.GetNginxVar(r, "$"+key)
	if key == "remote_addr" {
		if host, _, err := net.SplitHostPort(value); err == nil {
			return host
		}
	}

	return value
}
