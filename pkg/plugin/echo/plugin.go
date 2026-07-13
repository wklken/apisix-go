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
		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)

		p.rewrite(recorder)
		recorder.writeTo(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rewrite(resp *responseRecorder) {
	bodyChanged := false
	if p.config.Body != "" {
		resp.body = []byte(p.config.Body)
		bodyChanged = true
	} else {
		if p.config.BeforeBody != "" {
			resp.body = append([]byte(p.config.BeforeBody), resp.body...)
			bodyChanged = true
		}
		if p.config.AfterBody != "" {
			resp.body = append(resp.body, []byte(p.config.AfterBody)...)
			bodyChanged = true
		}
	}

	if bodyChanged {
		resp.header.Del("Content-Length")
	}

	for field, value := range p.config.Headers {
		resp.header.Set(field, fmt.Sprint(value))
	}
}

type responseRecorder struct {
	header      http.Header
	body        []byte
	statusCode  int
	wroteHeader bool
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.body = append(r.body, body...)
	return len(body), nil
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = w.Write(r.body)
}
