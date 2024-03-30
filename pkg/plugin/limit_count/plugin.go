package limit_count

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	limiter "github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config  Config
	limiter *limiter.Limiter
}

const (
	// version  = "0.1"
	priority = 1002
	name     = "limit-count"
)

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
	"required": ["count", "time_window"],
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
}

type RedisConfig struct {
	RedisHost      string `json:"redis_host,omitempty"`
	RedisPort      int    `json:"redis_port,omitempty"`
	RedisUsername  string `json:"redis_username,omitempty"`
	RedisPassword  string `json:"redis_password,omitempty"`
	RedisDatabase  int    `json:"redis_database,omitempty"`
	RedisTimeout   int    `json:"redis_timeout,omitempty"`
	RedisSSL       bool   `json:"redis_ssl,omitempty"`
	RedisSSLVerify bool   `json:"redis_ssl_verify,omitempty"`
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

	if p.config.AllowDegradation == nil {
		b := false
		p.config.AllowDegradation = &b
	}

	if p.config.ShowLimitQuotaHeader == nil {
		b := true
		p.config.ShowLimitQuotaHeader = &b
	}

	rate := limiter.Rate{
		Period: time.Duration(p.config.TimeWindow) * time.Second,
		Limit:  p.config.Count,
	}

	store := memory.NewStore()
	p.limiter = limiter.New(store, rate, limiter.WithTrustForwardHeader(true))

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func genKey(r *http.Request, key string) string {
	// FIXME: here is wrong, should use the context like nginx vars
	switch key {
	case "remote_addr":
		return r.Header.Get("X-Real-IP")
		// return r.RemoteAddr
	default:
		return r.Header.Get(key)
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// NOTE:  we got limit instance for each chain, so we don't need to worry about the key conflict in memory
		//        but we do share the same redis instance, so we need to make the namespace

		// FIXME: use the route_id?
		key := genKey(r, p.config.Key)

		fmt.Println("in limit-count, the key is: ", key)

		context, err := p.limiter.Get(r.Context(), key)
		if err != nil {
			// middleware.OnError(w, r, err)
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
			} else {
				http.Error(w, "failed to limit count", http.StatusInternalServerError)
			}
			return
		}

		if context.Reached {
			if *p.config.ShowLimitQuotaHeader {
				w.Header().Add("X-RateLimit-Limit", strconv.FormatInt(p.config.Count, 10))
				w.Header().Add("X-RateLimit-Remaining", "0")
				w.Header().Add("X-RateLimit-Reset", strconv.FormatInt(context.Reset, 10))
			}

			rejectedMsg := "Limit exceeded"
			if p.config.RejectedMsg != "" {
				rejectedMsg = p.config.RejectedMsg
			}
			http.Error(w, rejectedMsg, p.config.RejectedCode)
			return
		}

		if *p.config.ShowLimitQuotaHeader {
			w.Header().Add("X-RateLimit-Limit", strconv.FormatInt(context.Limit, 10))
			w.Header().Add("X-RateLimit-Remaining", strconv.FormatInt(context.Remaining, 10))
			w.Header().Add("X-RateLimit-Reset", strconv.FormatInt(context.Reset, 10))
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
