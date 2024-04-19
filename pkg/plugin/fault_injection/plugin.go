package fault_injection

import (
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

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
	Percentage int                    `json:"percentage,omitempty"`
	Headers    map[string]interface{} `json:"headers,omitempty"` // Note: interface{} due to oneOf {string, number}

	// FIXME: not support yet
	Vars [][]interface{} `json:"vars,omitempty"`
}

type Delay struct {
	Duration   float64 `json:"duration"`
	Percentage int     `json:"percentage,omitempty"`

	// FIXME: not support yet
	Vars [][]interface{} `json:"vars,omitempty"`
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
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.Delay != nil {
			if p.config.Delay.Duration > 0 && SampleHit(p.config.Delay.Percentage) {
				// sleep
				time.Sleep(time.Duration(p.config.Delay.Duration) * time.Second)
			}
		}

		if p.config.Abort != nil {
			if SampleHit(p.config.Abort.Percentage) {
				if p.config.Abort.Headers != nil {
					for k, v := range p.config.Abort.Headers {
						// FIXME: render the value with ctx.var
						w.Header().Set(k, fmt.Sprintf("%s", v))
					}
				}

				// FIXME: the body render with ctx.var
				http.Error(w, *p.config.Abort.Body, p.config.Abort.HTTPStatus)
				return
			}
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func SampleHit(percentage int) bool {
	if percentage == 0 {
		return true
	}

	return rand.Intn(100) < percentage
}
