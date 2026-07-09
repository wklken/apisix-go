package limit_req

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
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time

	redisLimiter reqLimiter
}

const (
	priority = 1001
	name     = "limit-req"
)

const schema = `
{
  "type": "object",
  "properties": {
    "rate": {
      "type": "number",
      "exclusiveMinimum": 0
    },
    "burst": {
      "type": "number",
      "minimum": 0
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
    "nodelay": {
      "type": "boolean",
      "default": false
    },
    "allow_degradation": {
      "type": "boolean",
      "default": false
    }
  },
  "required": ["rate", "burst", "key"]
}
`

type Config struct {
	Rate                  float64 `json:"rate"`
	Burst                 float64 `json:"burst"`
	Key                   string  `json:"key"`
	KeyType               string  `json:"key_type,omitempty"`
	Policy                string  `json:"policy,omitempty"`
	RedisHost             string  `json:"redis_host,omitempty"`
	RedisPort             int     `json:"redis_port,omitempty"`
	RedisUsername         string  `json:"redis_username,omitempty"`
	RedisPassword         string  `json:"redis_password,omitempty"`
	RedisDatabase         int     `json:"redis_database,omitempty"`
	RedisTimeout          int     `json:"redis_timeout,omitempty"`
	RedisSSL              *bool   `json:"redis_ssl,omitempty"`
	RedisSSLVerify        *bool   `json:"redis_ssl_verify,omitempty"`
	RedisKeepaliveTimeout int     `json:"redis_keepalive_timeout,omitempty"`
	RedisKeepalivePool    int     `json:"redis_keepalive_pool,omitempty"`
	RejectedCode          int     `json:"rejected_code,omitempty"`
	RejectedMsg           string  `json:"rejected_msg,omitempty"`
	Nodelay               *bool   `json:"nodelay,omitempty"`
	AllowDegradation      *bool   `json:"allow_degradation,omitempty"`

	rejectBody string
}

type bucket struct {
	excess float64
	last   time.Time
}

type reqLimiter interface {
	incoming(key string, rate float64, burst float64) (time.Duration, bool, error)
}

var varPattern = regexp.MustCompile(`\$\{([0-9A-Za-z_]+)\}|\$([0-9A-Za-z_]+)`)

const redisLimitReqScript = `
local state = redis.call("HMGET", KEYS[1], "excess", "last")
local excess = tonumber(state[1]) or 0
local last = tonumber(state[2]) or tonumber(ARGV[1])
local now = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local elapsed = (now - last) / 1000
excess = math.max(0, excess - elapsed * rate) + 1
local max_excess = burst + 1
local allowed = 1
if excess > max_excess then
  excess = max_excess
  allowed = 0
end

redis.call("HMSET", KEYS[1], "excess", excess, "last", now)
redis.call("PEXPIRE", KEYS[1], ttl)

local delay = 0
if allowed == 1 then
  delay = math.max(0, (excess - 1) / rate)
end

return {allowed, math.floor(delay * 1000)}
`

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Rate <= 0 {
		return fmt.Errorf("rate must be greater than 0")
	}

	if p.config.Burst < 0 {
		return fmt.Errorf("burst must be greater than or equal to 0")
	}

	if p.config.KeyType == "" {
		p.config.KeyType = "var"
	}

	if p.config.Policy == "" {
		p.config.Policy = "local"
	}
	if p.config.Policy != "local" && p.config.Policy != "redis" {
		return fmt.Errorf("not supported policy: %s", p.config.Policy)
	}
	if p.config.Policy == "redis" {
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
		if p.redisLimiter == nil {
			p.redisLimiter = p.newRedisLimiter()
		}
	}

	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = http.StatusServiceUnavailable
	}

	if p.config.Nodelay == nil {
		b := false
		p.config.Nodelay = &b
	}

	if p.config.AllowDegradation == nil {
		b := false
		p.config.AllowDegradation = &b
	}

	if p.config.RejectedMsg != "" {
		body, _ := json.Marshal(map[string]string{"error_msg": p.config.RejectedMsg})
		p.config.rejectBody = util.BytesToString(body)
	}

	if p.buckets == nil {
		p.buckets = make(map[string]*bucket)
	}
	if p.now == nil {
		p.now = time.Now
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		key := p.resolveKey(r)
		delay, allowed, err := p.incoming(key)
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "failed to limit req", http.StatusInternalServerError)
			return
		}
		if !allowed {
			if p.config.RejectedMsg != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(p.config.RejectedCode)
				_, _ = w.Write([]byte(p.config.rejectBody))
				return
			}
			w.WriteHeader(p.config.RejectedCode)
			return
		}

		if delay > 0 && !*p.config.Nodelay {
			time.Sleep(delay)
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) incoming(key string) (time.Duration, bool, error) {
	if p.config.Policy == "redis" {
		return p.redisLimiter.incoming(key, p.config.Rate, p.config.Burst)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	b, ok := p.buckets[key]
	if !ok {
		b = &bucket{last: now}
		p.buckets[key] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	b.excess = math.Max(0, b.excess-elapsed*p.config.Rate) + 1
	b.last = now

	maxExcess := p.config.Burst + 1
	if b.excess > maxExcess {
		b.excess = maxExcess
		return 0, false, nil
	}

	delaySeconds := (b.excess - 1) / p.config.Rate
	if delaySeconds <= 0 {
		return 0, true, nil
	}

	return time.Duration(delaySeconds * float64(time.Second)), true, nil
}

type redisReqLimiter struct {
	client *redis.Client
	now    func() time.Time
}

func (p *Plugin) newRedisLimiter() reqLimiter {
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
	if p.config.RedisSSL != nil && *p.config.RedisSSL {
		options.TLSConfig = &tls.Config{InsecureSkipVerify: !*p.config.RedisSSLVerify}
	}

	client := shared.LoadOrStoreClient(name, configUID, redis.NewClient(options)).(*redis.Client)
	return &redisReqLimiter{client: client, now: p.now}
}

func (l *redisReqLimiter) incoming(key string, rate float64, burst float64) (time.Duration, bool, error) {
	ttl := time.Duration(math.Ceil((burst+1)/rate)) * time.Second
	if ttl < time.Second {
		ttl = time.Second
	}
	now := l.now
	if now == nil {
		now = time.Now
	}

	result, err := l.client.Eval(
		context.Background(),
		redisLimitReqScript,
		[]string{"plugin-limit-req:" + key},
		now().UnixMilli(),
		rate,
		burst,
		ttl.Milliseconds(),
	).Result()
	if err != nil {
		return 0, false, err
	}

	values, ok := result.([]any)
	if !ok || len(values) != 2 {
		return 0, false, fmt.Errorf("unexpected redis limit-req result: %v", result)
	}
	allowed, ok := redisInt(values[0])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-req allowed value: %v", values[0])
	}
	delayMs, ok := redisInt(values[1])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-req delay value: %v", values[1])
	}

	return time.Duration(delayMs) * time.Millisecond, allowed == 1, nil
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
