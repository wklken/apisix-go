package limit_conn

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
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
  "oneOf": [
    {"required": ["conn", "burst", "default_conn_delay", "key"]},
    {"required": ["default_conn_delay", "rules"]}
  ]
}
`

type Config struct {
	Conn             any     `json:"conn"`
	Burst            any     `json:"burst"`
	DefaultConnDelay float64 `json:"default_conn_delay"`
	Key              string  `json:"key"`
	KeyType          string  `json:"key_type,omitempty"`
	Policy           string  `json:"policy,omitempty"`
	RejectedCode     int     `json:"rejected_code,omitempty"`
	RejectedMsg      string  `json:"rejected_msg,omitempty"`
	AllowDegradation *bool   `json:"allow_degradation,omitempty"`
	Rules            []Rule  `json:"rules,omitempty"`

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

var varPattern = regexp.MustCompile(`\$\{([0-9A-Za-z_]+)\}|\$([0-9A-Za-z_]+)`)

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

	if p.config.RejectedMsg != "" {
		body, _ := json.Marshal(map[string]string{"error_msg": p.config.RejectedMsg})
		p.config.rejectBody = util.BytesToString(body)
	}

	if p.conns == nil {
		p.conns = make(map[string]int)
	}

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

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if len(p.config.Rules) > 0 {
			admissions, delay, allowed := p.increaseRules(r)
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
			defer p.decreaseAdmissions(admissions)

			if delay > 0 {
				time.Sleep(delay)
			}

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
		delay, allowed := p.increase(key, conn, burst)
		if !allowed {
			p.reject(w)
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

func (p *Plugin) increaseRules(r *http.Request) ([]admission, time.Duration, bool) {
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

		nextDelay, allowed := p.increase(key, conn, burst)
		if !allowed {
			p.decreaseAdmissions(admissions)
			return nil, 0, false
		}
		admissions = append(admissions, admission{key: key})
		delay += nextDelay
	}

	return admissions, delay, true
}

func (p *Plugin) decreaseAdmissions(admissions []admission) {
	for _, admission := range admissions {
		p.decrease(admission.key)
	}
}

func (p *Plugin) increase(key string, conn int, burst int) (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current := p.conns[key] + 1
	limit := conn + burst
	if current > limit {
		return 0, false
	}

	p.conns[key] = current
	if current > conn {
		multiplier := (current - 1) / conn
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
