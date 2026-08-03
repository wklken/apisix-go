package traffic_label

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
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
                  "minimum": 0
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

var variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

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
	p.actionSequences = make([][]int, len(p.config.Rules))
	p.actionCursors = make([]int, len(p.config.Rules))

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
			for i := 0; i < weight; i++ {
				p.actionSequences[ruleIndex] = append(p.actionSequences[ruleIndex], actionIndex)
			}
		}
	}
	return nil
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
		resolved := fmt.Sprint(value)
		if stringValue, ok := value.(string); ok {
			resolved = resolveValue(r, stringValue)
		}
		r.Header.Set(name, resolved)
	}
}

func resolveValue(r *http.Request, value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return pluginexpr.String(pluginexpr.RequestValue(r, strings.TrimPrefix(variable, "$")))
	})
}
