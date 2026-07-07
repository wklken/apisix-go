package limit_conn

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu    sync.Mutex
	conns map[string]int
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
      "type": "integer",
      "exclusiveMinimum": 0
    },
    "burst": {
      "type": "integer",
      "minimum": 0
    },
    "default_conn_delay": {
      "type": "number",
      "exclusiveMinimum": 0
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
      "enum": ["local"],
      "default": "local"
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
    }
  },
  "required": ["conn", "burst", "default_conn_delay", "key"]
}
`

type Config struct {
	Conn             int     `json:"conn"`
	Burst            int     `json:"burst"`
	DefaultConnDelay float64 `json:"default_conn_delay"`
	Key              string  `json:"key"`
	KeyType          string  `json:"key_type,omitempty"`
	Policy           string  `json:"policy,omitempty"`
	RejectedCode     int     `json:"rejected_code,omitempty"`
	RejectedMsg      string  `json:"rejected_msg,omitempty"`
	AllowDegradation *bool   `json:"allow_degradation,omitempty"`
}

var varPattern = regexp.MustCompile(`\$\{([0-9A-Za-z_]+)\}|\$([0-9A-Za-z_]+)`)

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Conn <= 0 {
		return fmt.Errorf("conn must be greater than 0")
	}

	if p.config.Burst < 0 {
		return fmt.Errorf("burst must be greater than or equal to 0")
	}

	if p.config.DefaultConnDelay <= 0 {
		return fmt.Errorf("default_conn_delay must be greater than 0")
	}

	if p.config.KeyType == "" {
		p.config.KeyType = "var"
	}

	if p.config.Policy == "" {
		p.config.Policy = "local"
	}
	if p.config.Policy != "local" {
		return fmt.Errorf("not supported policy: %s", p.config.Policy)
	}

	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = http.StatusServiceUnavailable
	}

	if p.config.AllowDegradation == nil {
		b := false
		p.config.AllowDegradation = &b
	}

	if p.conns == nil {
		p.conns = make(map[string]int)
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		key := p.resolveKey(r)
		delay, allowed := p.increase(key)
		if !allowed {
			rejectedMsg := "Limit exceeded"
			if p.config.RejectedMsg != "" {
				rejectedMsg = p.config.RejectedMsg
			}
			http.Error(w, rejectedMsg, p.config.RejectedCode)
			return
		}
		defer p.decrease(key)

		if delay > 0 {
			time.Sleep(delay)
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) increase(key string) (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current := p.conns[key] + 1
	limit := p.config.Conn + p.config.Burst
	if current > limit {
		return 0, false
	}

	p.conns[key] = current
	if current > p.config.Conn {
		multiplier := (current - 1) / p.config.Conn
		return time.Duration(float64(multiplier) * p.config.DefaultConnDelay * float64(time.Second)), true
	}

	return 0, true
}

func (p *Plugin) decrease(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current := p.conns[key]
	if current <= 1 {
		delete(p.conns, key)
		return
	}
	p.conns[key] = current - 1
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
