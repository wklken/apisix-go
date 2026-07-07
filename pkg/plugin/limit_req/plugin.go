package limit_req

import (
	"fmt"
	"math"
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

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
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
	Rate             float64 `json:"rate"`
	Burst            float64 `json:"burst"`
	Key              string  `json:"key"`
	KeyType          string  `json:"key_type,omitempty"`
	Policy           string  `json:"policy,omitempty"`
	RejectedCode     int     `json:"rejected_code,omitempty"`
	RejectedMsg      string  `json:"rejected_msg,omitempty"`
	Nodelay          *bool   `json:"nodelay,omitempty"`
	AllowDegradation *bool   `json:"allow_degradation,omitempty"`
}

type bucket struct {
	excess float64
	last   time.Time
}

var varPattern = regexp.MustCompile(`\$\{([0-9A-Za-z_]+)\}|\$([0-9A-Za-z_]+)`)

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
	if p.config.Policy != "local" {
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
		delay, allowed := p.incoming(key)
		if !allowed {
			rejectedMsg := "Limit exceeded"
			if p.config.RejectedMsg != "" {
				rejectedMsg = p.config.RejectedMsg
			}
			http.Error(w, rejectedMsg, p.config.RejectedCode)
			return
		}

		if delay > 0 && !*p.config.Nodelay {
			time.Sleep(delay)
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) incoming(key string) (time.Duration, bool) {
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
		return 0, false
	}

	delaySeconds := (b.excess - 1) / p.config.Rate
	if delaySeconds <= 0 {
		return 0, true
	}

	return time.Duration(delaySeconds * float64(time.Second)), true
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
