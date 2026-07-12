package limit_count

import (
	"crypto/tls"
	"encoding/json"
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
	limiter "github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	sredis "github.com/ulule/limiter/v3/drivers/store/redis"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config       Config
	metadata     Metadata
	limiter      *limiter.Limiter
	limiterMu    sync.Mutex
	limiters     map[string]*limiter.Limiter
	ruleLimiters []*limiter.Limiter
	routeID      string
}

const (
	// version  = "0.1"
	priority = 1002
	name     = "limit-count"
)

var varPattern = regexp.MustCompile(`\$\{?[A-Za-z0-9_]+\}?`)

type limitCountGroup struct {
	fingerprint string
	store       limiter.Store
}

var limitCountGroups = struct {
	sync.Mutex
	entries map[string]limitCountGroup
}{entries: map[string]limitCountGroup{}}

const schema = `
{
	"type": "object",
	"properties": {
	  "count": {
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
	  "time_window": {
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
	  "rules": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "object",
		  "properties": {
			"count": {
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
			"time_window": {
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
	  },
	  "redis_config": {"$ref": "#/definitions/redis"},
	  "redis_cluster_config": {"$ref": "#/definitions/redis-cluster"}
	},
	"oneOf": [
	  {"required": ["count", "time_window"]},
	  {"required": ["rules"]}
	],
	"allOf": [
	  {
		"if": {
		  "properties": {"policy": {"const": "redis-cluster"}},
		  "required": ["policy"]
		},
		"then": {
		  "oneOf": [
			{"required": ["redis_cluster_nodes", "redis_cluster_name"]},
			{"required": ["redis_cluster_config"]}
		  ]
		}
	  }
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
	Count                 any                `json:"count"`
	TimeWindow            any                `json:"time_window"`
	Group                 string             `json:"group,omitempty"`
	Key                   string             `json:"key,omitempty"`
	KeyType               string             `json:"key_type,omitempty"`
	RejectedCode          int                `json:"rejected_code,omitempty"`
	RejectedMsg           string             `json:"rejected_msg,omitempty"`
	Policy                string             `json:"policy,omitempty"`
	AllowDegradation      *bool              `json:"allow_degradation,omitempty"`
	ShowLimitQuotaHeader  *bool              `json:"show_limit_quota_header,omitempty"`
	RedisHost             string             `json:"redis_host,omitempty"`
	RedisPort             int                `json:"redis_port,omitempty"`
	RedisUsername         string             `json:"redis_username,omitempty"`
	RedisPassword         string             `json:"redis_password,omitempty"`
	RedisDatabase         int                `json:"redis_database,omitempty"`
	RedisTimeout          int                `json:"redis_timeout,omitempty"`
	RedisSSL              *bool              `json:"redis_ssl,omitempty"`
	RedisSSLVerify        *bool              `json:"redis_ssl_verify,omitempty"`
	RedisKeepaliveTimeout int                `json:"redis_keepalive_timeout,omitempty"`
	RedisKeepalivePool    int                `json:"redis_keepalive_pool,omitempty"`
	RedisClusterNodes     []string           `json:"redis_cluster_nodes,omitempty"`
	RedisClusterName      string             `json:"redis_cluster_name,omitempty"`
	RedisClusterSSL       *bool              `json:"redis_cluster_ssl,omitempty"`
	RedisClusterSSLVerify *bool              `json:"redis_cluster_ssl_verify,omitempty"`
	Redis                 RedisConfig        `json:"redis_config"`
	RedisCluster          RedisClusterConfig `json:"redis_cluster_config"`
	Rules                 []Rule             `json:"rules,omitempty"`

	rejectBody string
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
	RedisClusterSSL       *bool    `json:"redis_cluster_ssl,omitempty"`
	RedisClusterSSLVerify *bool    `json:"redis_cluster_ssl_verify,omitempty"`
	RedisKeepaliveTimeout int      `json:"redis_keepalive_timeout,omitempty"`
	RedisKeepalivePool    int      `json:"redis_keepalive_pool,omitempty"`
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

	p.applyRootRedisConfig()
	p.applyRootRedisClusterConfig()
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
	} else if p.config.Policy == "redis-cluster" {
		if len(p.config.RedisCluster.RedisClusterNodes) == 0 {
			return fmt.Errorf("redis_cluster_nodes is required")
		}
		if p.config.RedisCluster.RedisClusterName == "" {
			return fmt.Errorf("redis_cluster_name is required")
		}
		if p.config.RedisCluster.RedisTimeout == 0 {
			p.config.RedisCluster.RedisTimeout = 1000
		}
		if p.config.RedisCluster.RedisClusterSSL == nil {
			value := false
			p.config.RedisCluster.RedisClusterSSL = &value
		}
		if p.config.RedisCluster.RedisClusterSSLVerify == nil {
			value := false
			p.config.RedisCluster.RedisClusterSSLVerify = &value
		}
		if p.config.RedisCluster.RedisKeepaliveTimeout == 0 {
			p.config.RedisCluster.RedisKeepaliveTimeout = 10000
		}
		if p.config.RedisCluster.RedisKeepalivePool == 0 {
			p.config.RedisCluster.RedisKeepalivePool = 100
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
	if p.config.RejectedMsg != "" {
		body, _ := json.Marshal(map[string]string{"error_msg": p.config.RejectedMsg})
		p.config.rejectBody = util.BytesToString(body)
	}
	if p.metadata == (Metadata{}) {
		p.metadata = loadMetadata()
	}

	if len(p.config.Rules) > 0 {
		if err := p.validateRules(); err != nil {
			return err
		}
		if err := p.registerGroup(); err != nil {
			return err
		}
		return p.initRuleLimiters()
	}

	count, countStatic, err := staticLimitValue(p.config.Count, "count")
	if err != nil {
		return err
	}
	timeWindow, timeWindowStatic, err := staticLimitValue(p.config.TimeWindow, "time_window")
	if err != nil {
		return err
	}
	if err := p.registerGroup(); err != nil {
		return err
	}
	if countStatic && timeWindowStatic {
		lim, err := p.newLimiter(count, timeWindow)
		if err != nil {
			return err
		}
		p.limiter = lim
	} else {
		p.limiters = make(map[string]*limiter.Limiter)
	}

	return nil
}

func (p *Plugin) SetResourceContext(route resource.Route, _ resource.Service) {
	p.routeID = route.ID
}

func (p *Plugin) scopedKey(key string) string {
	if p.config.Group != "" {
		return "group:" + p.config.Group + ":" + key
	}
	if p.routeID != "" {
		return "route:" + p.routeID + ":" + key
	}
	return "route:unknown:" + key
}

func (p *Plugin) registerGroup() error {
	if p.config.Group == "" {
		return nil
	}
	fingerprint, err := json.Marshal(p.config)
	if err != nil {
		return fmt.Errorf("marshal limit-count group config: %w", err)
	}

	limitCountGroups.Lock()
	defer limitCountGroups.Unlock()
	current, ok := limitCountGroups.entries[p.config.Group]
	if ok {
		if current.fingerprint != string(fingerprint) {
			return fmt.Errorf("group conf mismatched")
		}
		return nil
	}
	limitCountGroups.entries[p.config.Group] = limitCountGroup{
		fingerprint: string(fingerprint),
		store:       memory.NewStore(),
	}
	return nil
}

func (p *Plugin) localStore() limiter.Store {
	if p.config.Group == "" {
		return memory.NewStore()
	}
	limitCountGroups.Lock()
	defer limitCountGroups.Unlock()
	return limitCountGroups.entries[p.config.Group].store
}

func (p *Plugin) applyRootRedisConfig() {
	if p.config.Redis.RedisHost != "" {
		return
	}

	p.config.Redis.RedisHost = p.config.RedisHost
	p.config.Redis.RedisPort = p.config.RedisPort
	p.config.Redis.RedisUsername = p.config.RedisUsername
	p.config.Redis.RedisPassword = p.config.RedisPassword
	p.config.Redis.RedisDatabase = p.config.RedisDatabase
	p.config.Redis.RedisTimeout = p.config.RedisTimeout
	p.config.Redis.RedisSSL = p.config.RedisSSL
	p.config.Redis.RedisSSLVerify = p.config.RedisSSLVerify
}

func (p *Plugin) applyRootRedisClusterConfig() {
	if len(p.config.RedisCluster.RedisClusterNodes) > 0 {
		return
	}

	p.config.RedisCluster.RedisClusterNodes = append([]string(nil), p.config.RedisClusterNodes...)
	p.config.RedisCluster.RedisPassword = p.config.RedisPassword
	p.config.RedisCluster.RedisTimeout = p.config.RedisTimeout
	p.config.RedisCluster.RedisClusterName = p.config.RedisClusterName
	p.config.RedisCluster.RedisClusterSSL = p.config.RedisClusterSSL
	p.config.RedisCluster.RedisClusterSSLVerify = p.config.RedisClusterSSLVerify
	p.config.RedisCluster.RedisKeepaliveTimeout = p.config.RedisKeepaliveTimeout
	p.config.RedisCluster.RedisKeepalivePool = p.config.RedisKeepalivePool
}

func (p *Plugin) validateRules() error {
	seenKeys := make(map[string]struct{}, len(p.config.Rules))
	for _, rule := range p.config.Rules {
		if rule.Key == "" {
			return fmt.Errorf("limit-count rule key is required")
		}
		if _, ok := seenKeys[rule.Key]; ok {
			return fmt.Errorf("duplicate key %q in rules", rule.Key)
		}
		seenKeys[rule.Key] = struct{}{}

		if _, _, err := staticLimitValue(rule.Count, "rule count"); err != nil {
			return err
		}
		if _, _, err := staticLimitValue(rule.TimeWindow, "rule time_window"); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) initRuleLimiters() error {
	p.ruleLimiters = make([]*limiter.Limiter, len(p.config.Rules))
	for i, rule := range p.config.Rules {
		count, countStatic, err := staticLimitValue(rule.Count, "rule count")
		if err != nil {
			return err
		}
		timeWindow, timeWindowStatic, err := staticLimitValue(rule.TimeWindow, "rule time_window")
		if err != nil {
			return err
		}
		if countStatic && timeWindowStatic {
			lim, err := p.newLimiter(count, timeWindow)
			if err != nil {
				return err
			}
			p.ruleLimiters[i] = lim
		} else {
			p.limiters = make(map[string]*limiter.Limiter)
		}
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
		store = p.localStore()
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
		configUID := shared.NewConfigUID()
		configUID.Add(
			p.config.RedisCluster.RedisClusterName,
			strings.Join(p.config.RedisCluster.RedisClusterNodes, ","),
			p.config.RedisCluster.RedisPassword,
			p.config.RedisCluster.RedisTimeout,
			*p.config.RedisCluster.RedisClusterSSL,
			*p.config.RedisCluster.RedisClusterSSLVerify,
			p.config.RedisCluster.RedisKeepaliveTimeout,
			p.config.RedisCluster.RedisKeepalivePool,
		)
		client := shared.LoadOrStoreClient(
			name,
			configUID,
			redis.NewClusterClient(p.redisClusterOptions()),
		).(*redis.ClusterClient)

		var err error
		store, err = sredis.NewStoreWithOptions(client, limiter.StoreOptions{
			Prefix:   "limit-count",
			MaxRetry: 3,
		})
		if err != nil {
			return nil, err
		}
	}

	return limiter.New(store, rate, limiter.WithTrustForwardHeader(true)), nil
}

func (p *Plugin) redisClusterOptions() *redis.ClusterOptions {
	conf := p.config.RedisCluster
	options := &redis.ClusterOptions{
		Addrs:        append([]string(nil), conf.RedisClusterNodes...),
		Password:     conf.RedisPassword,
		DialTimeout:  time.Duration(conf.RedisTimeout) * time.Millisecond,
		ReadTimeout:  time.Duration(conf.RedisTimeout) * time.Millisecond,
		WriteTimeout: time.Duration(conf.RedisTimeout) * time.Millisecond,
		PoolSize:     conf.RedisKeepalivePool,
	}
	if conf.RedisKeepaliveTimeout > 0 {
		options.ConnMaxIdleTime = time.Duration(conf.RedisKeepaliveTimeout) * time.Millisecond
	}
	if conf.RedisClusterSSL != nil && *conf.RedisClusterSSL {
		options.TLSConfig = &tls.Config{InsecureSkipVerify: !*conf.RedisClusterSSLVerify}
	}
	return options
}

func staticLimitValue(value any, name string) (int64, bool, error) {
	if value == nil {
		return 0, false, fmt.Errorf("%s is required", name)
	}

	if expr, ok := value.(string); ok {
		if strings.Contains(expr, "$") {
			return 0, false, nil
		}
		parsed, err := strconv.ParseInt(expr, 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("%s must resolve to an integer: %w", name, err)
		}
		if parsed <= 0 {
			return 0, false, fmt.Errorf("%s must be greater than 0", name)
		}
		return parsed, true, nil
	}

	parsed, err := numericLimitValue(value, name)
	if err != nil {
		return 0, false, err
	}
	return parsed, true, nil
}

func resolveLimitValue(r *http.Request, value any, name string) (int64, error) {
	if expr, ok := value.(string); ok {
		resolved := varPattern.ReplaceAllStringFunc(expr, func(match string) string {
			varName := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
			varName = strings.TrimSuffix(varName, "}")
			return requestVar(r, varName)
		})
		parsed, err := strconv.ParseInt(resolved, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must resolve to an integer: %w", name, err)
		}
		if parsed <= 0 {
			return 0, fmt.Errorf("%s must be greater than 0", name)
		}
		return parsed, nil
	}

	return numericLimitValue(value, name)
}

func numericLimitValue(value any, name string) (int64, error) {
	switch v := value.(type) {
	case int:
		if v <= 0 {
			return 0, fmt.Errorf("%s must be greater than 0", name)
		}
		return int64(v), nil
	case int64:
		if v <= 0 {
			return 0, fmt.Errorf("%s must be greater than 0", name)
		}
		return v, nil
	case float64:
		if v <= 0 || math.Trunc(v) != v {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		return int64(v), nil
	case json.Number:
		parsed, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer: %w", name, err)
		}
		if parsed <= 0 {
			return 0, fmt.Errorf("%s must be greater than 0", name)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%s must be an integer or string expression", name)
	}
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if len(p.config.Rules) > 0 {
			applied := 0
			for i, rule := range p.config.Rules {
				key, ok := p.resolveRuleKey(r, rule)
				if !ok {
					continue
				}
				count, timeWindow, ok := p.resolveRuleLimit(r, rule)
				if !ok {
					continue
				}
				applied++
				lim := p.ruleLimiters[i]
				if lim == nil {
					var err error
					lim, err = p.limiterFor(count, timeWindow)
					if err != nil {
						if *p.config.AllowDegradation {
							continue
						}
						http.Error(w, "failed to limit count", http.StatusInternalServerError)
						return
					}
				}
				if !p.runLimit(w, r, lim, count, key, ruleHeaders(rule, i)) {
					return
				}
			}
			if applied == 0 && !*p.config.AllowDegradation {
				http.Error(w, "failed to resolve limit count rules", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		key := p.resolveKey(r)
		count, timeWindow, err := p.resolveLimit(r)
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "failed to resolve limit count config", http.StatusInternalServerError)
			return
		}
		lim, err := p.limiterFor(count, timeWindow)
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "failed to limit count", http.StatusInternalServerError)
			return
		}
		if !p.runLimit(w, r, lim, count, key, defaultHeaders(p.metadata)) {
			return
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) limiterFor(count int64, timeWindow int64) (*limiter.Limiter, error) {
	if p.limiter != nil {
		return p.limiter, nil
	}

	key := strconv.FormatInt(count, 10) + ":" + strconv.FormatInt(timeWindow, 10)
	p.limiterMu.Lock()
	defer p.limiterMu.Unlock()

	if p.limiters == nil {
		p.limiters = make(map[string]*limiter.Limiter)
	}
	lim, ok := p.limiters[key]
	if ok {
		return lim, nil
	}

	lim, err := p.newLimiter(count, timeWindow)
	if err != nil {
		return nil, err
	}
	p.limiters[key] = lim
	return lim, nil
}

func (p *Plugin) resolveLimit(r *http.Request) (int64, int64, error) {
	count, err := resolveLimitValue(r, p.config.Count, "count")
	if err != nil {
		return 0, 0, err
	}
	timeWindow, err := resolveLimitValue(r, p.config.TimeWindow, "time_window")
	if err != nil {
		return 0, 0, err
	}
	return count, timeWindow, nil
}

func (p *Plugin) resolveRuleLimit(r *http.Request, rule Rule) (int64, int64, bool) {
	count, err := resolveLimitValue(r, rule.Count, "rule count")
	if err != nil {
		return 0, 0, false
	}
	timeWindow, err := resolveLimitValue(r, rule.TimeWindow, "rule time_window")
	if err != nil {
		return 0, 0, false
	}
	return count, timeWindow, true
}

func (p *Plugin) runLimit(
	w http.ResponseWriter,
	r *http.Request,
	lim *limiter.Limiter,
	count int64,
	key string,
	headers quotaHeaders,
) bool {
	context, err := lim.Get(r.Context(), p.scopedKey(key))
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

		if p.config.RejectedMsg != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(p.config.RejectedCode)
			_, _ = w.Write([]byte(p.config.rejectBody))
			return false
		}
		w.WriteHeader(p.config.RejectedCode)
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

func defaultHeaders(metadata Metadata) quotaHeaders {
	metadata = applyMetadataDefaults(metadata)
	return quotaHeaders{
		limit:     metadata.LimitHeader,
		remaining: metadata.RemainingHeader,
		reset:     metadata.ResetHeader,
	}
}

func applyMetadataDefaults(metadata Metadata) Metadata {
	if metadata.LimitHeader == "" {
		metadata.LimitHeader = "X-RateLimit-Limit"
	}
	if metadata.RemainingHeader == "" {
		metadata.RemainingHeader = "X-RateLimit-Remaining"
	}
	if metadata.ResetHeader == "" {
		metadata.ResetHeader = "X-RateLimit-Reset"
	}
	return metadata
}

func loadMetadata() (metadata Metadata) {
	defer func() {
		if recover() != nil {
			metadata = Metadata{}
		}
	}()
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return Metadata{}
	}
	return metadata
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

	if after, ok := strings.CutPrefix(key, "http_"); ok {
		header := strings.ReplaceAll(after, "_", "-")
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
