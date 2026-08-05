package echo

import (
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 412
	name     = "echo"
)

const schema = `
{
  "type": "object",
  "properties": {
    "before_body": {
      "type": "string"
    },
    "body": {
      "type": "string"
    },
    "after_body": {
      "type": "string"
    },
    "headers": {
      "type": "object",
      "minProperties": 1,
      "additionalProperties": {
        "anyOf": [
          {"type": "string"},
          {"type": "number"}
        ]
      }
    }
  },
  "anyOf": [
    {
      "required": ["before_body"]
    },
    {
      "required": ["body"]
    },
    {
      "required": ["after_body"]
    }
  ],
  "minProperties": 1
}
`

type Config struct {
	BeforeBody string         `json:"before_body,omitempty"`
	Body       string         `json:"body,omitempty"`
	AfterBody  string         `json:"after_body,omitempty"`
	Headers    map[string]any `json:"headers,omitempty"`
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

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		recorder := base.GetOrCreateTransformResponseWriter(r)
		next.ServeHTTP(recorder, r)

		p.rewrite(recorder)
		recorder.Commit(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rewrite(resp *base.BufferedResponseWriter) {
	bodyChanged := false
	if p.config.Body != "" {
		resp.SetBody([]byte(p.config.Body))
		bodyChanged = true
	} else {
		if p.config.BeforeBody != "" {
			resp.SetBody(append([]byte(p.config.BeforeBody), resp.Body()...))
			bodyChanged = true
		}
		if p.config.AfterBody != "" {
			resp.SetBody(append(resp.Body(), []byte(p.config.AfterBody)...))
			bodyChanged = true
		}
	}

	if bodyChanged {
		resp.Header().Del("Content-Length")
	}

	for field, value := range p.config.Headers {
		resp.Header().Set(field, fmt.Sprint(value))
	}
}
