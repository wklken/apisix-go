package traffic_label

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
)

type Plugin struct {
	base.BasePlugin
	config Config

	actionPickers []weightedActionPicker
	lock          sync.Mutex
}

type weightedActionPicker struct {
	weights       []int
	lastAction    int
	currentWeight int
	maxWeight     int
	weightGCD     int
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
            "type": "array",
            "minItems": 1,
            "items": {
              "anyOf": [
                {"type": "array"},
                {"type": "string"}
              ]
            }
          },
          "actions": {
            "type": "array",
            "minItems": 1,
            "items": {
              "type": "object",
              "properties": {
                "set_headers": {
                  "type": "object",
                  "minProperties": 1,
                  "patternProperties": {
                    "^[^:]+$": {
                      "oneOf": [
                        {"type": "string"},
                        {"type": "number"}
                      ]
                    }
                  },
                  "additionalProperties": false
                },
                "weight": {
                  "type": "integer",
                  "default": 1,
                  "minimum": 1
                }
              },
              "additionalProperties": false
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
	expr    *pluginexpr.Expression
}

type Action struct {
	SetHeaders map[string]any `json:"set_headers,omitempty"`
	Weight     int            `json:"weight,omitempty"`
	weightSet  bool
}

func (a *Action) UnmarshalJSON(data []byte) error {
	var raw struct {
		SetHeaders map[string]any `json:"set_headers"`
		Weight     *int           `json:"weight"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	a.SetHeaders = raw.SetHeaders
	a.weightSet = raw.Weight != nil
	if raw.Weight != nil {
		a.Weight = *raw.Weight
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	p.actionPickers = make([]weightedActionPicker, len(p.config.Rules))

	for ruleIndex, rule := range p.config.Rules {
		if hasMixedLogicalOperators(rule.Match) {
			return fmt.Errorf("traffic-label rule %d contains mixed logical operators", ruleIndex)
		}
		expr, err := pluginexpr.Compile(rule.Match)
		if err != nil {
			return fmt.Errorf("traffic-label rule %d match validation failed: %w", ruleIndex, err)
		}
		p.config.Rules[ruleIndex].expr = expr
		for actionIndex, action := range rule.Actions {
			weight := action.Weight
			if weight == 0 && !action.weightSet {
				weight = 1
				p.config.Rules[ruleIndex].Actions[actionIndex].Weight = weight
			}
			if weight < 1 {
				return fmt.Errorf("traffic-label rule %d action %d weight must be at least 1", ruleIndex, actionIndex)
			}
			p.actionPickers[ruleIndex].weights = append(p.actionPickers[ruleIndex].weights, weight)
		}

		picker := &p.actionPickers[ruleIndex]
		if len(picker.weights) > 0 {
			picker.lastAction = rand.IntN(len(picker.weights))
			picker.maxWeight, picker.weightGCD = weightMetadata(picker.weights)
			picker.currentWeight = picker.maxWeight
		}
	}
	return nil
}

func weightMetadata(weights []int) (int, int) {
	maxWeight := 0
	weightGCD := 0
	for _, weight := range weights {
		if weight > maxWeight {
			maxWeight = weight
		}
		weightGCD = greatestCommonDivisor(weightGCD, weight)
	}
	return maxWeight, weightGCD
}

func greatestCommonDivisor(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func hasMixedLogicalOperators(match []any) bool {
	operator := ""
	for _, item := range match {
		value, ok := item.(string)
		if !ok {
			continue
		}
		value = strings.ToUpper(value)
		switch value {
		case "AND", "OR", "!AND", "!OR":
		default:
			continue
		}
		if operator != "" && operator != value {
			return true
		}
		operator = value
	}
	return false
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		for ruleIndex, rule := range p.config.Rules {
			if !rule.expr.Eval(func(name string) any {
				return pluginexpr.RequestValue(r, name)
			}) {
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
	if ruleIndex >= len(p.actionPickers) || len(p.actionPickers[ruleIndex].weights) == 0 {
		return nil
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	picker := &p.actionPickers[ruleIndex]
	for {
		picker.lastAction++
		if picker.lastAction >= len(picker.weights) {
			picker.lastAction = 0
			picker.currentWeight -= picker.weightGCD
			if picker.currentWeight <= 0 {
				picker.currentWeight = picker.maxWeight
			}
		}
		if picker.weights[picker.lastAction] >= picker.currentWeight {
			return &p.config.Rules[ruleIndex].Actions[picker.lastAction]
		}
	}
}

func applyAction(r *http.Request, action Action) {
	for name, value := range action.SetHeaders {
		resolved := fmt.Sprint(value)
		if stringValue, ok := value.(string); ok {
			resolved = resolveValue(r, stringValue)
		}
		r.Header.Set(name, resolved)
	}
}

func resolveValue(r *http.Request, value string) string {
	return base.ResolveRequestVariables(value, func(name string) string {
		return pluginexpr.String(pluginexpr.RequestValue(r, name))
	})
}
