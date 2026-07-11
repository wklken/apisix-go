package fault_injection

import (
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

var sleep = time.Sleep

const (
	// version  = "0.1"
	priority = 11000
	name     = "fault-injection"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "abort": {
		"type": "object",
		"properties": {
		  "http_status": {
			"type": "integer",
			"minimum": 200
		  },
		  "body": {
			"type": "string",
			"minLength": 0
		  },
		  "headers": {
			"type": "object",
			"minProperties": 1,
			"patternProperties": {
			  "^[^:]+$": {
				"oneOf": [
				  {
					"type": "string"
				  },
				  {
					"type": "number"
				  }
				]
			  }
			}
		  },
		  "percentage": {
			"type": "integer",
			"minimum": 0,
			"maximum": 100
		  },
		  "vars": {
			"type": "array",
			"maxItems": 20,
			"items": {
			  "type": "array"
			}
		  }
		},
		"required": ["http_status"]
	  },
	  "delay": {
		"type": "object",
		"properties": {
		  "duration": {
			"type": "number",
			"minimum": 0
		  },
		  "percentage": {
			"type": "integer",
			"minimum": 0,
			"maximum": 100
		  },
		  "vars": {
			"type": "array",
			"maxItems": 20,
			"items": {
			  "type": "array"
			}
		  }
		},
		"required": ["duration"]
	  }
	},
	"minProperties": 1
}`

type Abort struct {
	HTTPStatus int                    `json:"http_status"`
	Body       *string                `json:"body,omitempty"`
	Percentage *int                   `json:"percentage,omitempty"`
	Headers    map[string]interface{} `json:"headers,omitempty"` // Note: interface{} due to oneOf {string, number}

	Vars  [][]interface{} `json:"vars,omitempty"`
	exprs []*pluginexpr.Expression
}

type Delay struct {
	Duration   float64 `json:"duration"`
	Percentage *int    `json:"percentage,omitempty"`

	Vars  [][]interface{} `json:"vars,omitempty"`
	exprs []*pluginexpr.Expression
}

type Config struct {
	Abort *Abort `json:"abort"`
	Delay *Delay `json:"delay"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Abort != nil {
		exprs, err := compileVars(p.config.Abort.Vars)
		if err != nil {
			return fmt.Errorf("fault-injection abort vars validation failed: %w", err)
		}
		p.config.Abort.exprs = exprs
	}
	if p.config.Delay != nil {
		exprs, err := compileVars(p.config.Delay.Vars)
		if err != nil {
			return fmt.Errorf("fault-injection delay vars validation failed: %w", err)
		}
		p.config.Delay.exprs = exprs
	}
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.Delay != nil {
			if p.config.Delay.Duration > 0 && SampleHit(p.config.Delay.Percentage) &&
				varsMatch(r, p.config.Delay.exprs) {
				// sleep
				sleep(time.Duration(p.config.Delay.Duration * float64(time.Second)))
			}
		}

		if p.config.Abort != nil {
			if SampleHit(p.config.Abort.Percentage) && varsMatch(r, p.config.Abort.exprs) {
				if p.config.Abort.Headers != nil {
					for k, v := range p.config.Abort.Headers {
						value := fmt.Sprint(v)
						if stringValue, ok := v.(string); ok {
							value = resolveValue(r, stringValue)
						}
						w.Header().Set(k, value)
					}
				}

				w.WriteHeader(p.config.Abort.HTTPStatus)
				if p.config.Abort.Body != nil {
					_, _ = w.Write([]byte(resolveValue(r, *p.config.Abort.Body)))
				}
				return
			}
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func SampleHit(percentage *int) bool {
	if percentage == nil {
		return true
	}

	return rand.Intn(100) < *percentage
}

func compileVars(vars [][]interface{}) ([]*pluginexpr.Expression, error) {
	exprs := make([]*pluginexpr.Expression, 0, len(vars))
	for i, rule := range vars {
		expr, err := pluginexpr.Compile([]any(rule))
		if err != nil {
			return nil, fmt.Errorf("vars item %d: %w", i, err)
		}
		exprs = append(exprs, expr)
	}
	return exprs, nil
}

func varsMatch(r *http.Request, exprs []*pluginexpr.Expression) bool {
	if len(exprs) == 0 {
		return true
	}
	for _, expr := range exprs {
		if expr.Eval(func(name string) any {
			return pluginexpr.RequestValue(r, name)
		}) {
			return true
		}
	}
	return false
}

var variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

func resolveValue(r *http.Request, value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return pluginexpr.String(pluginexpr.RequestValue(r, strings.TrimPrefix(variable, "$")))
	})
}
