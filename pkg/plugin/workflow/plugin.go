package workflow

import (
	"fmt"
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/plugin/limit_conn"
	"github.com/wklken/apisix-go/pkg/plugin/limit_count"
	"github.com/wklken/apisix-go/pkg/plugin/limit_req"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 1006
	name     = "workflow"
)

const schema = `
{
  "type": "object",
  "properties": {
    "rules": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "case": {
            "type": "array",
            "items": {
              "anyOf": [
                {
                  "type": "array"
                },
                {
                  "type": "string"
                }
              ]
            },
            "minItems": 1
          },
          "actions": {
            "type": "array",
            "items": {
              "type": "array",
              "minItems": 1
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
	Case    []any    `json:"case,omitempty"`
	Actions []Action `json:"actions,omitempty"`
	expr    *pluginexpr.Expression
}

type Action struct {
	Name       string
	Config     map[string]any
	Return     ReturnAction
	limitConn  *limit_conn.Plugin
	limitCount *limit_count.Plugin
	limitReq   *limit_req.Plugin
}

type ReturnAction struct {
	Code int `json:"code,omitempty"`
}

func (a *Action) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("workflow action must contain a name")
	}
	if err := json.Unmarshal(raw[0], &a.Name); err != nil {
		return err
	}
	if len(raw) > 1 {
		if err := json.Unmarshal(raw[1], &a.Config); err != nil {
			return err
		}
	}
	if a.Name == "return" && len(raw) > 1 {
		return util.Parse(a.Config, &a.Return)
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
	for ruleIndex := range p.config.Rules {
		rule := &p.config.Rules[ruleIndex]
		if len(rule.Case) > 0 {
			expr, err := pluginexpr.Compile(rule.Case)
			if err != nil {
				return fmt.Errorf("workflow rule %d case validation failed: %w", ruleIndex, err)
			}
			rule.expr = expr
		}
		for actionIndex := range p.config.Rules[ruleIndex].Actions {
			action := &p.config.Rules[ruleIndex].Actions[actionIndex]
			switch action.Name {
			case "limit-req":
				plugin := &limit_req.Plugin{}
				if err := plugin.Init(); err != nil {
					return err
				}
				if err := util.Parse(action.Config, plugin.Config()); err != nil {
					return err
				}
				if err := plugin.PostInit(); err != nil {
					return err
				}
				action.limitReq = plugin
			case "limit-conn":
				plugin := &limit_conn.Plugin{}
				if err := plugin.Init(); err != nil {
					return err
				}
				if err := util.Parse(action.Config, plugin.Config()); err != nil {
					return err
				}
				if err := plugin.PostInit(); err != nil {
					return err
				}
				action.limitConn = plugin
			case "limit-count":
				plugin := &limit_count.Plugin{}
				if err := plugin.Init(); err != nil {
					return err
				}
				if _, ok := action.Config["group"]; ok {
					return fmt.Errorf("workflow rule %d limit-count action group is not supported", ruleIndex)
				}
				compiledSchema, err := util.CompileSchema(plugin.GetSchema())
				if err != nil {
					return fmt.Errorf("workflow rule %d limit-count action validation failed: %w", ruleIndex, err)
				}
				if err := compiledSchema.Validate(action.Config); err != nil {
					return fmt.Errorf("workflow rule %d limit-count action validation failed: %w", ruleIndex, err)
				}
				if err := util.Parse(action.Config, plugin.Config()); err != nil {
					return err
				}
				if err := plugin.PostInit(); err != nil {
					return err
				}
				action.limitCount = plugin
			case "return":
				if action.Return.Code < http.StatusContinue || action.Return.Code > 599 {
					return fmt.Errorf(
						"workflow return action code must be between %d and 599",
						http.StatusContinue,
					)
				}
			default:
				return fmt.Errorf("unsupported workflow action %q", action.Name)
			}
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// One resolver per request: rule matching never allocates a closure
		// per step, so scanning hundreds of rules stays allocation-free.
		resolve := func(name string) any {
			return pluginexpr.RequestValue(r, name)
		}
		for _, rule := range p.config.Rules {
			if !matchRule(r, rule, resolve) {
				continue
			}
			if p.handleAction(w, r, next, rule.Actions) {
				return
			}
			break
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) handleAction(w http.ResponseWriter, r *http.Request, next http.Handler, actions []Action) bool {
	if len(actions) == 0 {
		return false
	}
	action := actions[0]
	if action.Name == "limit-req" && action.limitReq != nil {
		r = withConsumerActionOverride(r, action.Name)
		action.limitReq.Handler(next).ServeHTTP(w, r)
		return true
	}
	if action.Name == "limit-conn" && action.limitConn != nil {
		r = withConsumerActionOverride(r, action.Name)
		action.limitConn.Handler(next).ServeHTTP(w, r)
		return true
	}
	if action.Name == "limit-count" && action.limitCount != nil {
		r = withConsumerActionOverride(r, action.Name)
		action.limitCount.Handler(next).ServeHTTP(w, r)
		return true
	}

	if action.Name != "return" {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(action.Return.Code)
	_, _ = w.Write([]byte(`{"error_msg":"rejected by workflow"}`))
	return true
}

func withConsumerActionOverride(r *http.Request, actionName string) *http.Request {
	if !apisixctx.ConsumerPluginOverrides(r, name) {
		return r
	}
	consumer, ok := apisixctx.GetApisixVar(r, "$consumer").(resource.Consumer)
	if !ok {
		return r
	}

	overrides := make(map[string]struct{}, len(consumer.Plugins)+1)
	if consumer.GroupID != "" {
		if group, err := store.GetConsumerGroup(consumer.GroupID); err == nil {
			for pluginName := range group.Plugins {
				overrides[pluginName] = struct{}{}
			}
		}
	}
	for pluginName := range consumer.Plugins {
		overrides[pluginName] = struct{}{}
	}
	overrides[actionName] = struct{}{}
	return apisixctx.WithConsumerPluginOverrides(r, overrides)
}

func matchRule(r *http.Request, rule Rule, resolve pluginexpr.Resolver) bool {
	if len(rule.Case) == 0 {
		return true
	}
	if rule.expr == nil {
		return false
	}
	return rule.expr.Eval(resolve)
}
