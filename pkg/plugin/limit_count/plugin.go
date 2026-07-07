package limit_count

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	sredis "github.com/ulule/limiter/v3/drivers/store/redis"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config       Config
	limiter      *limiter.Limiter
	ruleLimiters []*limiter.Limiter
}

const (
	// version  = "0.1"
	priority = 1002
	name     = "limit-count"
)

var varPattern = regexp.MustCompile(`\$\{?[A-Za-z0-9_]+\}?`)

const schema = `
{
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
	  "allow_degradation": {
		"type": "boolean",
		"default": false
	  },
	  "show_limit_quota_header": {
		"type": "boolean",
		"default": true
	  },
	  "redis_config": {"$ref": "#/definitions/redis"},
	  "redis_cluster_config": {"$ref": "#/definitions/redis-cluster"}
	},
	"oneOf": [
	  {"required": ["count", "time_window"]},
	  {"required": ["rules"]}
	],
	"definitions": {
	  "redis": {
			"properties": {
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
			  }
			},
			"required": ["redis_host"]
	  },
	  "redis-cluster": {
		"properties": {
		  "redis_cluster_nodes": {
			"type": "array",
			"minItems": 1,
			"items": {
			  "type": "string",
			  "minLength": 2,
			  "maxLength": 100
			}
		  },
		  "redis_password": {
			"type": "string",
			"minLength": 0
		  },
		  "redis_timeout": {
			"type": "integer",
			"minimum": 1,
			"default": 1000
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
		  }
		},
		"required": ["redis_cluster_nodes", "redis_cluster_name"]
	  }
	}
  }
`

type Config struct {
	Count                int64              `json:"count"`
	TimeWindow           int64              `json:"time_window"`
	Group                string             `json:"group,omitempty"`
	Key                  string             `json:"key,omitempty"`
	KeyType              string             `json:"key_type,omitempty"`
	RejectedCode         int                `json:"rejected_code,omitempty"`
	RejectedMsg          string             `json:"rejected_msg,omitempty"`
	Policy               string             `json:"policy,omitempty"`
	AllowDegradation     *bool              `json:"allow_degradation,omitempty"`
	ShowLimitQuotaHeader *bool              `json:"show_limit_quota_header,omitempty"`
	Redis                RedisConfig        `json:"redis_config,omitempty"`
	RedisCluster         RedisClusterConfig `json:"redis_cluster_config,omitempty"`
	Rules                []Rule             `json:"rules,omitempty"`
}

type Rule struct {
	Count        int64  `json:"count"`
	TimeWindow   int64  `json:"time_window"`
	Key          string `json:"key"`
	HeaderPrefix string `json:"header_prefix,omitempty"`
}

type RedisConfig struct {
	RedisHost      string `json:"redis_host,omitempty"`
	RedisPort      int    `json:"redis_port,omitempty"`
	RedisUsername  string `json:"redis_username,omitempty"`
	RedisPassword  string `json:"redis_password,omitempty"`
	RedisDatabase  int    `json:"redis_database,omitempty"`
	RedisTimeout   int    `json:"redis_timeout,omitempty"`
	RedisSSL       *bool  `json:"redis_ssl,omitempty"`
	RedisSSLVerify *bool  `json:"redis_ssl_verify,omitempty"`
}

func (rc *RedisConfig) String() string {
	c, _ := json.Marshal(rc)
	return util.BytesToString(c)
}

// RedisClusterConfig holds fields specific to the "redis-cluster" policy.
type RedisClusterConfig struct {
	RedisClusterNodes     []string `json:"redis_cluster_nodes,omitempty"`
	RedisPassword         string   `json:"redis_password,omitempty"`
	RedisTimeout          int      `json:"redis_timeout,omitempty"`
	RedisClusterName      string   `json:"redis_cluster_name,omitempty"`
	RedisClusterSSL       bool     `json:"redis_cluster_ssl,omitempty"`
	RedisClusterSSLVerify bool     `json:"redis_cluster_ssl_verify,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Key == "" {
		p.config.Key = "remote_addr"
	}
	if p.config.KeyType == "" {
		p.config.KeyType = "var"
	}

	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = 503
	}

	if p.config.Policy == "" {
		p.config.Policy = "local"
	}

	if p.config.Policy == "redis" {
		if p.config.Redis.RedisPort == 0 {
			p.config.Redis.RedisPort = 6379
		}

		// if p.config.Redis.RedisDatabase == 0 {
		// 	p.config.Redis.RedisDatabase = 0
		// }

		if p.config.Redis.RedisTimeout == 0 {
			p.config.Redis.RedisTimeout = 1000
		}

		if p.config.Redis.RedisSSL == nil {
			b := false
			p.config.Redis.RedisSSL = &b
		}

		if p.config.Redis.RedisSSLVerify == nil {
			b := false
			p.config.Redis.RedisSSLVerify = &b
		}

	}

	if p.config.AllowDegradation == nil {
		b := false
		p.config.AllowDegradation = &b
	}

	if p.config.ShowLimitQuotaHeader == nil {
		b := true
		p.config.ShowLimitQuotaHeader = &b
	}

	if len(p.config.Rules) > 0 {
		return p.initRuleLimiters()
	}

	lim, err := p.newLimiter(p.config.Count, p.config.TimeWindow)
	if err != nil {
		return err
	}
	p.limiter = lim

	return nil
}

func (p *Plugin) initRuleLimiters() error {
	seenKeys := make(map[string]struct{}, len(p.config.Rules))
	p.ruleLimiters = make([]*limiter.Limiter, len(p.config.Rules))
	for i, rule := range p.config.Rules {
		if rule.Key == "" {
			return fmt.Errorf("limit-count rule key is required")
		}
		if _, ok := seenKeys[rule.Key]; ok {
			return fmt.Errorf("duplicate key %q in rules", rule.Key)
		}
		seenKeys[rule.Key] = struct{}{}

		lim, err := p.newLimiter(rule.Count, rule.TimeWindow)
		if err != nil {
			return err
		}
		p.ruleLimiters[i] = lim
	}
	return nil
}

func (p *Plugin) newLimiter(count int64, timeWindow int64) (*limiter.Limiter, error) {
	rate := limiter.Rate{
		Period: time.Duration(timeWindow) * time.Second,
		Limit:  count,
	}

	var store limiter.Store
	if p.config.Policy == "local" {
		store = memory.NewStore()
	} else if p.config.Policy == "redis" {
		// each route has its own limit => we should share the redis client
		configUID := shared.NewConfigUID()
		configUID.Add(p.config.Redis.String())
		c := redis.NewClient(&redis.Options{
			Addr:     fmt.Sprintf("%s:%d", p.config.Redis.RedisHost, p.config.Redis.RedisPort),
			Username: p.config.Redis.RedisUsername,
			Password: p.config.Redis.RedisPassword,
			DB:       p.config.Redis.RedisDatabase,
			// RedisTimeout   int    `json:"redis_timeout,omitempty"`
			// RedisSSL       bool   `json:"redis_ssl,omitempty"`
			// RedisSSLVerify bool   `json:"redis_ssl_verify,omitempty"`
		})
		client := shared.LoadOrStoreClient(name, configUID, c).(*redis.Client)

		// BREAKPOINT: add redis into docker-compose, then test it
		var err error
		store, err = sredis.NewStoreWithOptions(client, limiter.StoreOptions{
			Prefix:   "limit-count",
			MaxRetry: 3,
		})
		// TODO: handle the error
		if err != nil {
			return nil, err
		}
	} else if p.config.Policy == "redis-cluster" {
		// client := redis.NewClusterClient(&redis.ClusterOptions{
		// 	Addrs:    p.config.RedisCluster.RedisClusterNodes,
		// 	Password: p.config.RedisCluster.RedisPassword,
		// })
		// RedisTimeout          int      `json:"redis_timeout,omitempty"`
		// RedisClusterName      string   `json:"redis_cluster_name,omitempty"`
		// RedisClusterSSL       bool     `json:"redis_cluster_ssl,omitempty"`
		// RedisClusterSSLVerify bool     `json:"redis_cluster_ssl_verify,omitempty"`

		return nil, fmt.Errorf("not supported yet: %s", p.config.Policy)
	}

	return limiter.New(store, rate, limiter.WithTrustForwardHeader(true)), nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// NOTE:  we got limit instance for each chain, so we don't need to worry about the key conflict in memory
		//        but we do share the same redis instance, so we need to make the namespace

		if len(p.config.Rules) > 0 {
			for i, rule := range p.config.Rules {
				key, ok := p.resolveRuleKey(r, rule)
				if !ok {
					continue
				}
				if !p.runLimit(w, r, p.ruleLimiters[i], rule.Count, key, ruleHeaders(rule, i)) {
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}

		key := p.resolveKey(r)
		if !p.runLimit(w, r, p.limiter, p.config.Count, key, defaultHeaders()) {
			return
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) runLimit(
	w http.ResponseWriter,
	r *http.Request,
	lim *limiter.Limiter,
	count int64,
	key string,
	headers quotaHeaders,
) bool {
	context, err := lim.Get(r.Context(), key)
	if err != nil {
		if *p.config.AllowDegradation {
			return true
		}
		http.Error(w, "failed to limit count", http.StatusInternalServerError)
		return false
	}

	if context.Reached {
		if *p.config.ShowLimitQuotaHeader {
			w.Header().Add(headers.limit, strconv.FormatInt(count, 10))
			w.Header().Add(headers.remaining, "0")
			w.Header().Add(headers.reset, strconv.FormatInt(context.Reset, 10))
		}

		rejectedMsg := "Limit exceeded"
		if p.config.RejectedMsg != "" {
			rejectedMsg = p.config.RejectedMsg
		}
		http.Error(w, rejectedMsg, p.config.RejectedCode)
		return false
	}

	if *p.config.ShowLimitQuotaHeader {
		w.Header().Add(headers.limit, strconv.FormatInt(context.Limit, 10))
		w.Header().Add(headers.remaining, strconv.FormatInt(context.Remaining, 10))
		w.Header().Add(headers.reset, strconv.FormatInt(context.Reset, 10))
	}

	return true
}

func (p *Plugin) resolveKey(r *http.Request) string {
	var key string
	switch p.config.KeyType {
	case "constant":
		key = p.config.Key
	case "var_combination":
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
	default:
		key = requestVar(r, p.config.Key)
	}

	if key == "" {
		key = requestVar(r, "remote_addr")
	}
	return key
}

func (p *Plugin) resolveRuleKey(r *http.Request, rule Rule) (string, bool) {
	resolved := 0
	key := varPattern.ReplaceAllStringFunc(rule.Key, func(match string) string {
		name := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
		name = strings.TrimSuffix(name, "}")
		value := requestVar(r, name)
		if value != "" {
			resolved++
		}
		return value
	})
	if resolved == 0 || key == "" {
		return "", false
	}
	return key, true
}

type quotaHeaders struct {
	limit     string
	remaining string
	reset     string
}

func defaultHeaders() quotaHeaders {
	return quotaHeaders{
		limit:     "X-RateLimit-Limit",
		remaining: "X-RateLimit-Remaining",
		reset:     "X-RateLimit-Reset",
	}
}

func ruleHeaders(rule Rule, index int) quotaHeaders {
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
