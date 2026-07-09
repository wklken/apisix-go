package traffic_label

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	actionSequences [][]int
	actionCursors   []int
	lock            sync.Mutex
}

const (
	priority = 967
	name     = "traffic-label"
)

const schema = `
{
  "type": "object",
  "properties": {
    "rules": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "match": {
            "type": "array"
          },
          "actions": {
            "type": "array",
            "minItems": 1,
            "items": {
              "type": "object",
              "properties": {
                "set_headers": {
                  "type": "object",
                  "minProperties": 1
                },
                "weight": {
                  "type": "integer",
                  "default": 1,
                  "minimum": 1
                }
              }
            }
          }
        },
        "required": ["actions"]
      }
    }
  },
  "required": ["rules"]
}
`

type Config struct {
	Rules []Rule `json:"rules,omitempty"`
}

type Rule struct {
	Match   []any    `json:"match,omitempty"`
	Actions []Action `json:"actions,omitempty"`
}

type Action struct {
	SetHeaders map[string]string `json:"set_headers,omitempty"`
	Weight     int               `json:"weight,omitempty"`
}

var variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	p.actionSequences = make([][]int, len(p.config.Rules))
	p.actionCursors = make([]int, len(p.config.Rules))

	for ruleIndex, rule := range p.config.Rules {
		for actionIndex, action := range rule.Actions {
			weight := action.Weight
			if weight == 0 {
				weight = 1
				p.config.Rules[ruleIndex].Actions[actionIndex].Weight = weight
			}
			for i := 0; i < weight; i++ {
				p.actionSequences[ruleIndex] = append(p.actionSequences[ruleIndex], actionIndex)
			}
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		for ruleIndex, rule := range p.config.Rules {
			if !matchRule(r, rule.Match) {
				continue
			}

			action := p.nextAction(ruleIndex)
			if action != nil {
				applyAction(r, *action)
			}
			break
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) nextAction(ruleIndex int) *Action {
	if ruleIndex >= len(p.actionSequences) || len(p.actionSequences[ruleIndex]) == 0 {
		return nil
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	sequence := p.actionSequences[ruleIndex]
	cursor := p.actionCursors[ruleIndex] % len(sequence)
	p.actionCursors[ruleIndex]++

	actionIndex := sequence[cursor]
	if actionIndex >= len(p.config.Rules[ruleIndex].Actions) {
		return nil
	}
	return &p.config.Rules[ruleIndex].Actions[actionIndex]
}

func applyAction(r *http.Request, action Action) {
	for name, value := range action.SetHeaders {
		r.Header.Set(name, resolveValue(r, value))
	}
}

func matchRule(r *http.Request, conditions []any) bool {
	if len(conditions) == 0 {
		return true
	}

	pendingOp := "AND"
	hasResult := false
	result := true
	for _, condition := range conditions {
		if op, ok := condition.(string); ok {
			switch strings.ToUpper(op) {
			case "AND", "OR":
				pendingOp = strings.ToUpper(op)
			default:
				return false
			}
			continue
		}

		matched := matchCondition(r, condition)
		if !hasResult {
			result = matched
			hasResult = true
			continue
		}

		if pendingOp == "OR" {
			result = result || matched
		} else {
			result = result && matched
		}
		pendingOp = "AND"
	}
	return hasResult && result
}

func matchCondition(r *http.Request, condition any) bool {
	parts, ok := condition.([]any)
	if !ok || len(parts) != 3 {
		return false
	}

	left := fmt.Sprint(parts[0])
	op := fmt.Sprint(parts[1])
	right := fmt.Sprint(parts[2])
	actual := requestVar(r, left)

	switch op {
	case "==":
		return actual == right
	case "!=":
		return actual != right
	case ">":
		return compareNumber(actual, right, func(a, b float64) bool { return a > b })
	case ">=":
		return compareNumber(actual, right, func(a, b float64) bool { return a >= b })
	case "<":
		return compareNumber(actual, right, func(a, b float64) bool { return a < b })
	case "<=":
		return compareNumber(actual, right, func(a, b float64) bool { return a <= b })
	case "~":
		matched, _ := regexp.MatchString(right, actual)
		return matched
	case "!~":
		matched, _ := regexp.MatchString(right, actual)
		return !matched
	default:
		return false
	}
}

func compareNumber(left string, right string, compare func(float64, float64) bool) bool {
	l, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return false
	}
	r, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return false
	}
	return compare(l, r)
}

func resolveValue(r *http.Request, value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return requestVar(r, strings.TrimPrefix(variable, "$"))
	})
}

func requestVar(r *http.Request, name string) string {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "method", name == "request_method":
		return r.Method
	case name == "host":
		return r.Host
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
}
