package workflow

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limit_conn"
	"github.com/wklken/apisix-go/pkg/plugin/limit_count"
	"github.com/wklken/apisix-go/pkg/plugin/limit_req"
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
	for ruleIndex := range p.config.Rules {
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
				if err := util.Parse(action.Config, plugin.Config()); err != nil {
					return err
				}
				if err := plugin.PostInit(); err != nil {
					return err
				}
				action.limitCount = plugin
			}
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		for _, rule := range p.config.Rules {
			if !matchRule(r, rule.Case) {
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
		action.limitReq.Handler(next).ServeHTTP(w, r)
		return true
	}
	if action.Name == "limit-conn" && action.limitConn != nil {
		action.limitConn.Handler(next).ServeHTTP(w, r)
		return true
	}
	if action.Name == "limit-count" && action.limitCount != nil {
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
	default:
		return false
	}
}

func requestVar(r *http.Request, name string) string {
	switch {
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "method":
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
