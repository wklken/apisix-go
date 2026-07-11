package traffic_label

import (
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
	SetHeaders map[string]interface{} `json:"set_headers,omitempty"`
	Weight     int                    `json:"weight,omitempty"`
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
		expr, err := pluginexpr.Compile(rule.Match)
		if err != nil {
			return fmt.Errorf("traffic-label rule %d match validation failed: %w", ruleIndex, err)
		}
		p.config.Rules[ruleIndex].expr = expr
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
