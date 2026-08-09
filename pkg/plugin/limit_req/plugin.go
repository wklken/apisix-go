package limit_req

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu      sync.Mutex
	buckets *cacheutil.BoundedTTLMap[*bucket]
	now     func() time.Time

	bucketTTL time.Duration

	redisLimiter reqLimiter
	routeID      string

	clientRelease    func()
	consumerStoreMu  sync.Mutex
	consumerStoreKey string
	consumerStore    *bucketStore
}

// defaultLocalBucketsCapacity bounds the number of in-memory local-policy
// buckets; the earliest expiring buckets are evicted once the bound is hit.
var defaultLocalBucketsCapacity = 10000

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
  "required": ["rate", "burst", "key"],
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
	Rate                  float64  `json:"rate"`
	Burst                 float64  `json:"burst"`
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
	RejectedCode          int      `json:"rejected_code,omitempty"`
	RejectedMsg           string   `json:"rejected_msg,omitempty"`
	Nodelay               *bool    `json:"nodelay,omitempty"`
	AllowDegradation      *bool    `json:"allow_degradation,omitempty"`

	rejectBody string
}

type bucket struct {
	excess float64
	last   time.Time
}

type bucketStore struct {
	mu      sync.Mutex
	buckets *cacheutil.BoundedTTLMap[*bucket]
}

type reqLimiter interface {
	incoming(key string, rate float64, burst float64) (time.Duration, bool, error)
}

type consumerBucketEntry struct {
	store *bucketStore
	refs  int
}

var consumerBucketStores = struct {
	sync.Mutex
	entries map[string]consumerBucketEntry
}{entries: map[string]consumerBucketEntry{}}

const redisLimitReqScript = `
local state = redis.call("HMGET", KEYS[1], "excess", "last")
local excess = tonumber(state[1]) or 0
local last = tonumber(state[2]) or tonumber(ARGV[1])
local now = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local elapsed = math.max(0, (now - last) / 1000)
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
		if p.redisLimiter == nil {
			p.redisLimiter = p.newRedisClusterLimiter()
		}
	default:
		return fmt.Errorf("not supported policy: %s", p.config.Policy)
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
		body, err := json.Marshal(map[string]string{"error_msg": p.config.RejectedMsg})
		if err != nil {
			return fmt.Errorf("limit-req failed to marshal rejected_msg: %w", err)
		}
		p.config.rejectBody = util.BytesToString(body)
	}

	if p.buckets == nil {
		p.buckets = cacheutil.NewBoundedTTLMap[*bucket](
			defaultLocalBucketsCapacity,
			func() time.Time { return p.now() },
		)
	}
	if p.now == nil {
		p.now = time.Now
	}
	p.bucketTTL = max(time.Duration(math.Ceil((p.config.Burst+1)/p.config.Rate))*time.Second, time.Second)

	return nil
}

func (p *Plugin) Stop() {
	p.releaseConsumerBucketStore()
	if p.clientRelease != nil {
		p.clientRelease()
		p.clientRelease = nil
	}
}

func (p *Plugin) Config() any {
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
		key := p.resolveKey(r)
		consumerName := ""
		if apisixctx.ConsumerPluginOverrides(r, name) {
			consumerName, _ = apisixctx.GetApisixVar(r, "$consumer_name").(string)
		}
		delay, allowed, err := p.incomingWithConsumer(key, consumerName)
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			logger.Errorf("failed to limit req: %v", err)
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

func (p *Plugin) incomingWithConsumer(key string, consumerName string) (time.Duration, bool, error) {
	if consumerName == "" {
		key = p.scopedKey(key)
	} else {
		key = "consumer:" + consumerName + ":" + key
	}
	if p.config.Policy == "redis" || p.config.Policy == "redis-cluster" {
		return p.redisLimiter.incoming(key, p.config.Rate, p.config.Burst)
	}

	mu := &p.mu
	var buckets *cacheutil.BoundedTTLMap[*bucket]
	if consumerName != "" {
		store := p.consumerBucketStore()
		mu = &store.mu
		buckets = store.buckets
	} else {
		buckets = p.buckets
	}
	mu.Lock()
	defer mu.Unlock()

	now := p.now()
	b, ok := buckets.Get(key)
	if !ok {
		b = &bucket{last: now}
		buckets.Set(key, b, p.bucketTTL)
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

func (p *Plugin) consumerBucketStore() *bucketStore {
	p.consumerStoreMu.Lock()
	defer p.consumerStoreMu.Unlock()
	if p.consumerStore != nil {
		return p.consumerStore
	}

	uid := shared.NewConfigUID()
	uid.Add(p.config.Rate, p.config.Burst, p.config.Key, p.config.KeyType)
	key := uid.String()
	consumerBucketStores.Lock()
	entry, ok := consumerBucketStores.entries[key]
	if !ok {
		entry = consumerBucketEntry{
			store: &bucketStore{
				buckets: cacheutil.NewBoundedTTLMap[*bucket](
					defaultLocalBucketsCapacity,
					func() time.Time { return p.now() },
				),
			},
		}
	}
	entry.refs++
	consumerBucketStores.entries[key] = entry
	consumerBucketStores.Unlock()

	p.consumerStoreKey = key
	p.consumerStore = entry.store
	return p.consumerStore
}

func (p *Plugin) releaseConsumerBucketStore() {
	p.consumerStoreMu.Lock()
	key, store := p.consumerStoreKey, p.consumerStore
	p.consumerStoreKey = ""
	p.consumerStore = nil
	p.consumerStoreMu.Unlock()
	if key == "" || store == nil {
		return
	}

	consumerBucketStores.Lock()
	entry, ok := consumerBucketStores.entries[key]
	if ok && entry.store == store {
		entry.refs--
		if entry.refs <= 0 {
			delete(consumerBucketStores.entries, key)
		} else {
			consumerBucketStores.entries[key] = entry
		}
	}
	consumerBucketStores.Unlock()
}

func (p *Plugin) resolveKey(r *http.Request) string {
	var key string
	if p.config.KeyType == "var_combination" {
		resolved := 0
		key = limitbase.VarPattern.ReplaceAllStringFunc(p.config.Key, func(match string) string {
			name := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
			name = strings.TrimSuffix(name, "}")
			value := base.RequestVarFromNginx(r, name)
			if value != "" {
				resolved++
			}
			return value
		})
		if resolved == 0 {
			key = ""
		}
	} else {
		key = base.RequestVarFromNginx(r, p.config.Key)
	}

	if key == "" {
		logger.Warn("The value of the configured key is empty, use client IP instead")
		key = base.RequestVarFromNginx(r, "remote_addr")
	}
	return key
}
