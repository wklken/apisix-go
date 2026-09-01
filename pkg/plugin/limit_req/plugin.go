package limit_req

import (
	"fmt"
	"net/http"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	apisixContext  base.APISIXPluginContext
	rateLimitState *limitbase.State
	redisLimiter   reqLimiter
	routeID        string
	clientRelease  func()
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
      "type": "string"
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

type reqLimiter interface {
	incoming(key string, rate float64, burst float64) (time.Duration, bool, error)
}

const redisLimitReqScript = `
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])
local state = redis.call("HMGET", KEYS[1], "excess", "last")
local excess

if state[1] and state[2] then
  local elapsed = now - tonumber(state[2])
  excess = math.max(tonumber(state[1]) - rate * math.abs(elapsed) / 1000 + 1000, 0)
  if excess > burst then
    return {0, tostring(excess)}
  end
else
  excess = 0
end

redis.call("HSET", KEYS[1], "excess", excess, "last", now)
redis.call("EXPIRE", KEYS[1], ttl)
return {1, tostring(excess)}
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
		p.config.rejectBody = util.BytesToString(body) + "\n"
	}

	if p.rateLimitState == nil {
		p.rateLimitState = limitbase.NewState()
	}

	return nil
}

func (p *Plugin) Stop() {
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

func (p *Plugin) SetRateLimitState(state *limitbase.State) {
	p.rateLimitState = state
}

func (p *Plugin) SetAPISIXPluginContext(pluginContext base.APISIXPluginContext) error {
	p.apisixContext = pluginContext.Clone()
	return nil
}

func (p *Plugin) scopedKey(key string) string {
	if p.apisixContext.SourceConfig != nil {
		document := cloneLimitReqMap(p.apisixContext.SourceConfig)
		if p.apisixContext.WorkflowVID > 0 {
			document["_vid"] = p.apisixContext.WorkflowVID
		}
		if scoped, err := buildLimitReqKey(p.apisixContext, document, key); err == nil {
			return scoped
		}
	}
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
			w.WriteHeader(http.StatusInternalServerError)
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
	if p.apisixContext.SourceConfig != nil || consumerName == "" {
		key = p.scopedKey(key)
	} else {
		key = "consumer:" + consumerName + ":" + key
	}
	logger.Infof("limit key: %s", key)
	if p.config.Policy == "redis" || p.config.Policy == "redis-cluster" {
		return p.redisLimiter.incoming(key, p.config.Rate, p.config.Burst)
	}

	result := p.rateLimitState.LeakyBucket(key, p.config.Rate, p.config.Burst)
	return result.Delay, result.Allowed, nil
}

func (p *Plugin) resolveKey(r *http.Request) string {
	var key string
	if p.config.KeyType == "var_combination" {
		var resolved int
		key, resolved = limitbase.ResolveVars(p.config.Key, func(name string) string {
			return base.RequestVarFromNginx(r, name)
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
