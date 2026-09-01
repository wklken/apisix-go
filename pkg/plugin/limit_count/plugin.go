package limit_count

import (
	"crypto/sha256"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/util"

	limiter "github.com/ulule/limiter/v3"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type Plugin struct {
	base.BasePlugin
	limitCountSecretState
	config            Config
	metadata          Metadata
	limiter           *limiter.Limiter
	limiterMu         sync.Mutex
	limiters          map[string]*limiter.Limiter
	ruleLimiters      []*limiter.Limiter
	routeID           string
	apisixContext     base.APISIXPluginContext
	rateLimitState    *limitbase.State
	localLimiterStore limiter.Store
	fixedStore        limiter.Store
	dynamicLimits     bool
	groupRegistered   bool

	backendMu     sync.Mutex
	backendClient redis.UniversalClient
	clientRelease func()
}

const (
	// version  = "0.1"
	priority = 1002
	name     = "limit-count"
)

var varPattern = regexp.MustCompile(`\$\{?[A-Za-z0-9_]+\}?`)

const maxSafeInteger = int64(1<<53 - 1)

type limitCountGroup struct {
	fingerprint string
	store       limiter.Store
	refs        int
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
			"type": "string"
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
			"type": "string"
		  }
		]
	  },
	  "rules": {
		"type": "array",
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
				  "type": "string"
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
				  "type": "string"
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
	  }
	},
	"oneOf": [
	  {"required": ["count", "time_window"]},
	  {"required": ["rules"]}
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

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "limit_header": {
      "type": "string"
    },
    "remaining_header": {
      "type": "string"
    },
    "reset_header": {
      "type": "string"
    }
  }
}
`

type Config struct {
	Count                 any      `json:"count"`
	TimeWindow            any      `json:"time_window"`
	Group                 string   `json:"group,omitempty"`
	Key                   string   `json:"key,omitempty"`
	KeyType               string   `json:"key_type,omitempty"`
	RejectedCode          int      `json:"rejected_code,omitempty"`
	RejectedMsg           string   `json:"rejected_msg,omitempty"`
	Policy                string   `json:"policy,omitempty"`
	AllowDegradation      *bool    `json:"allow_degradation,omitempty"`
	ShowLimitQuotaHeader  *bool    `json:"show_limit_quota_header,omitempty"`
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
	Rules                 []Rule   `json:"rules,omitempty"`

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

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	return nil
}

func (p *Plugin) PostInit() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.credentialMu.Lock()
	retired := p.retired
	p.credentialMu.Unlock()
	if retired {
		return secret.ErrCredentialUnavailable
	}
	var metadata Metadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("limit-count metadata decode failed: %w", err)
	}
	p.metadata = metadata

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
	switch p.config.Policy {
	case "redis":
		if p.config.RedisPort == 0 {
			p.config.RedisPort = 6379
		}
		if p.config.RedisTimeout == 0 {
			p.config.RedisTimeout = 1000
		}
		if p.config.RedisKeepaliveTimeout == 0 {
			p.config.RedisKeepaliveTimeout = 10000
		}
		if p.config.RedisKeepalivePool == 0 {
			p.config.RedisKeepalivePool = 100
		}
		if p.config.RedisSSL == nil {
			b := false
			p.config.RedisSSL = &b
		}
		if p.config.RedisSSLVerify == nil {
			b := false
			p.config.RedisSSLVerify = &b
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
	if p.config.Rules != nil {
		if err := p.validateRules(); err != nil {
			return err
		}
		if err := p.registerGroup(); err != nil {
			return err
		}
		if err := p.initRuleLimiters(); err != nil {
			p.releaseGroup()
			return err
		}
		return nil
	}

	count, countStatic, err := staticLimitValue(p.config.Count, "count")
	if err != nil {
		return err
	}
	timeWindow, timeWindowStatic, err := staticLimitValue(p.config.TimeWindow, "time_window")
	if err != nil {
		return err
	}
	p.dynamicLimits = !countStatic || !timeWindowStatic
	if err := p.registerGroup(); err != nil {
		return err
	}
	if countStatic && timeWindowStatic {
		if p.config.Policy == "local" {
			lim, err := p.newLimiter(count, timeWindow)
			if err != nil {
				p.releaseGroup()
				return err
			}
			p.limiter = lim
		} else {
			p.limiters = make(map[string]*limiter.Limiter)
		}
	} else {
		p.limiters = make(map[string]*limiter.Limiter)
	}

	return nil
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
	if p.hasAPISIXPluginContext() {
		if document, err := p.effectiveLimitCountDocument(); err == nil {
			if scoped, err := BuildLocalKey(p.apisixContext, document, key); err == nil {
				return scoped
			}
		}
	}
	if p.config.Group != "" {
		return "group:" + p.config.Group + ":" + key
	}
	if p.routeID != "" {
		return "route:" + p.routeID + ":" + key
	}
	return "route:unknown:" + key
}

func (p *Plugin) consumerScopedKey(r *http.Request, key string) string {
	if p.hasAPISIXPluginContext() {
		return key
	}
	if !apisixctx.ConsumerPluginOverrides(r, name) {
		return key
	}
	consumerName, _ := apisixctx.GetApisixVar(r, "$consumer_name").(string)
	if consumerName == "" {
		return key
	}
	return "consumer:" + consumerName + ":" + key
}

func (p *Plugin) registerGroup() error {
	if p.config.Group == "" || p.groupRegistered {
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
		current.refs++
		limitCountGroups.entries[p.config.Group] = current
		p.groupRegistered = true
		return nil
	}
	limitCountGroups.entries[p.config.Group] = limitCountGroup{
		fingerprint: string(fingerprint),
		store:       newLocalFixedWindowStore(time.Now, defaultLocalStoreCapacity),
		refs:        1,
	}
	p.groupRegistered = true
	return nil
}

func (p *Plugin) releaseGroup() {
	if !p.groupRegistered || p.config.Group == "" {
		return
	}
	limitCountGroups.Lock()
	entry, ok := limitCountGroups.entries[p.config.Group]
	if ok {
		entry.refs--
		if entry.refs <= 0 {
			delete(limitCountGroups.entries, p.config.Group)
		} else {
			limitCountGroups.entries[p.config.Group] = entry
		}
	}
	limitCountGroups.Unlock()
	p.groupRegistered = false
}

func (p *Plugin) localStore() limiter.Store {
	if p.rateLimitState != nil {
		return newLimitCountStateStore(p.rateLimitState)
	}
	if p.config.Group == "" {
		if p.localLimiterStore == nil {
			p.localLimiterStore = newLocalFixedWindowStore(time.Now, defaultLocalStoreCapacity)
		}
		return p.localLimiterStore
	}
	limitCountGroups.Lock()
	defer limitCountGroups.Unlock()
	return limitCountGroups.entries[p.config.Group].store
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
		if !countStatic || !timeWindowStatic {
			p.dynamicLimits = true
		}
		if countStatic && timeWindowStatic && p.config.Policy == "local" {
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
	switch p.config.Policy {
	case "local":
		store = p.localStore()
	case "redis", "redis-cluster":
		var err error
		store, err = p.fixedWindowStore()
		if err != nil {
			return nil, err
		}
	}

	return limiter.New(store, rate, limiter.WithTrustForwardHeader(true)), nil
}

func (p *Plugin) fixedWindowStore() (limiter.Store, error) {
	p.backendMu.Lock()
	defer p.backendMu.Unlock()
	if p.fixedStore != nil {
		return p.fixedStore, nil
	}

	client, err := p.redisBackendClientLocked()
	if err != nil {
		return nil, err
	}
	store := newRedisLimitCountStore(client, time.Now)
	p.fixedStore = store
	return store, nil
}

func (p *Plugin) redisBackendClient() (redis.UniversalClient, error) {
	p.backendMu.Lock()
	defer p.backendMu.Unlock()
	return p.redisBackendClientLocked()
}

func (p *Plugin) redisBackendClientLocked() (redis.UniversalClient, error) {
	if p.backendClient != nil {
		return p.backendClient, nil
	}

	switch p.config.Policy {
	case "redis":
		var client redis.UniversalClient
		err := p.withLimitCountRedisHost(func(host string) error {
			hostDigest := sha256.Sum256([]byte(host))
			configUID := shared.NewConfigUID()
			configUID.Add(
				p.config.Policy,
				fmt.Sprintf("sha256:%x", hostDigest),
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
			runtimeConfig := p.redisConnConfig()
			runtimeConfig.Host = host
			options := runtimeConfig.Options()
			var err error
			client, err = p.acquireLimitCountRedisClient(
				configUID, func() (any, error) { return redis.NewClient(options), nil },
			)
			return err
		})
		return client, err
	case "redis-cluster":
		var client redis.UniversalClient
		err := p.withLimitCountRedisNodes(func(nodes []string) error {
			identityNodes := make([]string, len(nodes))
			for index, node := range nodes {
				digest := sha256.Sum256([]byte(node))
				identityNodes[index] = fmt.Sprintf("sha256:%x", digest)
			}
			configUID := shared.NewConfigUID()
			configUID.Add(
				p.config.Policy,
				p.config.RedisClusterName,
				strings.Join(identityNodes, ","),
				p.config.RedisPassword,
				p.config.RedisTimeout,
				*p.config.RedisClusterSSL,
				*p.config.RedisClusterSSLVerify,
				p.config.RedisKeepaliveTimeout,
				p.config.RedisKeepalivePool,
			)
			runtimeConfig := p.redisClusterConnConfig()
			runtimeConfig.Nodes = append([]string(nil), nodes...)
			options := runtimeConfig.ClusterOptions()
			var err error
			client, err = p.acquireLimitCountRedisClient(
				configUID, func() (any, error) { return redis.NewClusterClient(options), nil },
			)
			return err
		})
		return client, err
	default:
		return nil, fmt.Errorf("policy %q has no Redis backend", p.config.Policy)
	}
}

func (p *Plugin) acquireLimitCountRedisClient(
	configUID *shared.ConfigUID, create func() (any, error),
) (redis.UniversalClient, error) {
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		create,
		shared.CloseRedisClient,
	)
	if err != nil {
		return nil, err
	}
	p.backendClient = value.(redis.UniversalClient)
	p.clientRelease = release
	return p.backendClient, nil
}

func staticLimitValue(value any, name string) (int64, bool, error) {
	if value == nil {
		return 0, false, fmt.Errorf("%s is required", name)
	}

	if expr, ok := value.(string); ok {
		if expr == "" || strings.Contains(expr, "$") {
			return 0, false, nil
		}
		parsed, err := parseLimitInt(expr, name)
		if err != nil {
			return 0, false, err
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
		if match := limitbase.DefaultVarPattern.FindStringSubmatch(expr); match != nil {
			resolved := base.RequestVarFromNginx(r, match[1])
			if resolved == "" {
				resolved = strings.TrimSpace(match[2])
			}
			return parseLimitInt(resolved, name)
		}

		resolved := varPattern.ReplaceAllStringFunc(expr, func(match string) string {
			varName := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
			varName = strings.TrimSuffix(varName, "}")
			return base.RequestVarFromNginx(r, varName)
		})
		return parseLimitInt(resolved, name)
	}

	return numericLimitValue(value, name)
}

func numericLimitValue(value any, name string) (int64, error) {
	switch v := value.(type) {
	case int:
		parsed := int64(v)
		if err := validateLimitInt(parsed, name); err != nil {
			return 0, err
		}
		return parsed, nil
	case int64:
		if err := validateLimitInt(v, name); err != nil {
			return 0, err
		}
		return v, nil
	case float64:
		if math.Trunc(v) != v {
			return 0, fmt.Errorf("%s resolved value must be an integer", name)
		}
		if v > float64(maxSafeInteger) || v < float64(-maxSafeInteger) {
			return 0, fmt.Errorf("%s resolved value exceeds safe integer range", name)
		}
		parsed := int64(v)
		if err := validateLimitInt(parsed, name); err != nil {
			return 0, err
		}
		return parsed, nil
	case json.Number:
		return parseLimitInt(string(v), name)
	default:
		return 0, fmt.Errorf("%s must be an integer or string expression", name)
	}
}

func parseLimitInt(value string, name string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s resolved value must be an integer: %w", name, err)
	}
	if err := validateLimitInt(parsed, name); err != nil {
		return 0, err
	}
	return parsed, nil
}

func validateLimitInt(value int64, name string) error {
	if value > maxSafeInteger || value < -maxSafeInteger {
		return fmt.Errorf("%s resolved value exceeds safe integer range", name)
	}
	if value <= 0 {
		return fmt.Errorf("%s resolved value must be a positive number", name)
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.Rules != nil {
			applied := 0
			for i, rule := range p.config.Rules {
				key, ok := p.resolveRuleKey(r, rule)
				if !ok {
					continue
				}
				key = p.consumerScopedKey(r, key)
				logger.Debugf("limit key: %s", key)
				count, timeWindow, err := p.resolveRuleLimit(r, rule)
				if err != nil {
					logger.Error(err.Error())
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
						writeLimitCountBackendError(w)
						return
					}
				}
				if !p.runLimit(w, r, lim, count, key, limitbase.RuleQuotaHeaders(rule.HeaderPrefix, i)) {
					return
				}
			}
			if applied == 0 && !*p.config.AllowDegradation {
				logger.Error("failed to get rate limit rules")
				http.Error(w, "failed to resolve limit count rules", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		proceed := false
		err := p.withResolvedLimitCountKey(r, func(key string) error {
			proceed = p.consumeLimitCountKey(w, r, p.consumerScopedKey(r, key))
			return nil
		})
		if err != nil {
			if p.config.AllowDegradation != nil && *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			logger.Errorf("failed to resolve limit count key: %v", err)
			http.Error(w, "failed to resolve limit count config", http.StatusInternalServerError)
			return
		}
		if proceed {
			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) consumeLimitCountKey(w http.ResponseWriter, r *http.Request, key string) bool {
	count, timeWindow, err := p.resolveLimit(r)
	if err != nil {
		if *p.config.AllowDegradation {
			return true
		}
		logger.Error(err.Error())
		http.Error(w, "failed to resolve limit count config", http.StatusInternalServerError)
		return false
	}
	lim, err := p.limiterFor(count, timeWindow)
	if err != nil {
		if *p.config.AllowDegradation {
			return true
		}
		logger.Errorf("failed to limit count: %v", err)
		writeLimitCountBackendError(w)
		return false
	}
	if !p.runLimit(
		w,
		r,
		lim,
		count,
		key,
		limitbase.DefaultQuotaHeaders(p.metadata.LimitHeader, p.metadata.RemainingHeader, p.metadata.ResetHeader),
	) {
		return false
	}
	return true
}

func (p *Plugin) limiterFor(count int64, timeWindow int64) (*limiter.Limiter, error) {
	if p.limiter != nil {
		return p.limiter, nil
	}

	p.limiterMu.Lock()
	defer p.limiterMu.Unlock()
	if p.dynamicLimits {
		return p.newLimiter(count, timeWindow)
	}

	key := strconv.FormatInt(count, 10) + ":" + strconv.FormatInt(timeWindow, 10)
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

func (p *Plugin) resolveRuleLimit(r *http.Request, rule Rule) (int64, int64, error) {
	count, err := resolveLimitValue(r, rule.Count, "rule count")
	if err != nil {
		return 0, 0, err
	}
	timeWindow, err := resolveLimitValue(r, rule.TimeWindow, "rule time_window")
	if err != nil {
		return 0, 0, err
	}
	return count, timeWindow, nil
}

func (p *Plugin) runLimit(
	w http.ResponseWriter,
	r *http.Request,
	lim *limiter.Limiter,
	count int64,
	key string,
	headers limitbase.QuotaHeaders,
) bool {
	context, err := lim.Get(r.Context(), p.scopedKey(key))
	if err != nil {
		if *p.config.AllowDegradation {
			return true
		}
		logger.Errorf("failed to limit count: %v", err)
		writeLimitCountBackendError(w)
		return false
	}
	reset := fixedWindowResetSeconds(context.Reset, time.Now())
	p.recordRateLimitingInfo(r, key, context.Limit, context.Remaining, reset)

	if context.Reached {
		if *p.config.ShowLimitQuotaHeader {
			w.Header().Add(headers.Limit, strconv.FormatInt(count, 10))
			w.Header().Add(headers.Remaining, "0")
			w.Header().Add(headers.Reset, strconv.FormatInt(reset, 10))
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
		w.Header().Add(headers.Limit, strconv.FormatInt(context.Limit, 10))
		w.Header().Add(headers.Remaining, strconv.FormatInt(context.Remaining, 10))
		w.Header().Add(headers.Reset, strconv.FormatInt(reset, 10))
	}

	return true
}

func writeLimitCountBackendError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error_msg":"failed to limit count"}`))
}

func fixedWindowResetSeconds(expiration int64, now time.Time) int64 {
	return max(expiration-now.Unix(), 0)
}

func (p *Plugin) recordRateLimitingInfo(
	r *http.Request,
	key string,
	limit int64,
	remaining int64,
	reset any,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$rate_limiting_info", map[string]any{
		"rate_limiting_key":       p.scopedKey(key),
		"rate_limiting_limit":     limit,
		"rate_limiting_remaining": remaining,
		"rate_limiting_reset":     reset,
	})
}

func (p *Plugin) Stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.stopOnce.Do(func() {
		p.stopLimitCount()
	})
}

func (p *Plugin) stopLimitCount() {
	p.credentialMu.Lock()
	p.retired = true
	wait := p.usesDone
	p.credentialMu.Unlock()
	if wait != nil {
		<-wait
	}

	p.releaseGroup()

	p.limiterMu.Lock()
	p.limiter = nil
	p.limiters = nil
	p.ruleLimiters = nil
	p.localLimiterStore = nil
	p.dynamicLimits = false
	p.limiterMu.Unlock()

	p.backendMu.Lock()
	release := p.clientRelease
	p.clientRelease = nil
	p.backendClient = nil
	p.fixedStore = nil
	p.backendMu.Unlock()
	if release != nil {
		release()
	}

	p.credentialMu.Lock()
	p.scopedKeySecret = secret.Value{}
	p.scopedRedisHost = secret.Value{}
	p.scopedRedisClusterNodes = nil
	p.keyPresent = false
	p.redisHostPresent = false
	p.redisNodesPresent = false
	p.scopedSet = false
	p.keyField = ""
	p.redisHostField = ""
	p.redisNodesField = ""
	p.credentialMu.Unlock()
}

func (p *Plugin) withResolvedLimitCountKey(
	r *http.Request, use func(string) error,
) error {
	return p.withLimitCountKey(func(configuredKey string) error {
		return use(resolveLimitCountKey(r, p.config.KeyType, configuredKey))
	})
}

func resolveLimitCountKey(r *http.Request, keyType, configuredKey string) string {
	var key string
	switch keyType {
	case "constant":
		key = configuredKey
	case "var_combination":
		resolved := 0
		key = varPattern.ReplaceAllStringFunc(configuredKey, func(match string) string {
			name := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
			name = strings.TrimSuffix(name, "}")
			value := limitCountRequestVar(r, name)
			if value != "" {
				resolved++
			}
			return value
		})
		if resolved == 0 {
			key = ""
		}
	default:
		key = limitCountRequestVar(r, configuredKey)
	}

	if key == "" {
		key = limitCountRequestVar(r, "remote_addr")
	}
	return key
}

func limitCountRequestVar(r *http.Request, name string) string {
	name = strings.TrimPrefix(name, "$")
	if argument, ok := strings.CutPrefix(name, "arg_"); ok {
		return r.URL.Query().Get(argument)
	}
	if name == "http_host" {
		return r.Host
	}
	return base.RequestVarFromNginx(r, name)
}

func (p *Plugin) resolveRuleKey(r *http.Request, rule Rule) (string, bool) {
	if match := limitbase.DefaultVarPattern.FindStringSubmatch(rule.Key); match != nil {
		key := limitCountRequestVar(r, match[1])
		if key == "" {
			key = strings.TrimSpace(match[2])
		}
		return key, key != ""
	}

	resolved := 0
	key := varPattern.ReplaceAllStringFunc(rule.Key, func(match string) string {
		name := strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$")
		name = strings.TrimSuffix(name, "}")
		value := limitCountRequestVar(r, name)
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
