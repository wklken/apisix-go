package echo

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
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
    },
    {
      "required": ["headers"]
    }
  ],
  "minProperties": 1
}
`

type Config struct {
	BeforeBody string         `json:"before_body,omitempty"`
	Body       *string        `json:"body,omitempty"`
	AfterBody  string         `json:"after_body,omitempty"`
	Headers    map[string]any `json:"headers,omitempty"`

	beforeBodySet bool
	afterBodySet  bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type configAlias Config
	var decoded configAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	decoded.beforeBodySet = fields["before_body"] != nil
	decoded.afterBodySet = fields["after_body"] != nil
	*c = Config(decoded)
	return nil
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

// DescribeBindingPhases selects header and body callbacks independently. Echo
// never owns a request stage.
func (p Config) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	body := p.BeforeBody != "" || p.Body != nil || p.AfterBody != "" || p.beforeBodySet || p.afterBodySet
	header := len(p.Headers) > 0
	if !body && !header {
		return base.BindingPhaseDescriptor{}, errors.New(
			"echo requires a header or body field",
		)
	}
	return base.BindingPhaseDescriptor{
		RequestStage: "none",
		Header:       header,
		BufferedBody: body,
	}, nil
}

func (p *Plugin) RunHeaderFilter(_ *http.Request, state *base.ResponseState) error {
	if state == nil {
		return nil
	}
	if state.Header == nil {
		state.Header = make(http.Header)
	}
	for field, value := range p.config.Headers {
		state.Header.Set(field, fmt.Sprint(value))
	}
	return nil
}

func (p *Plugin) RunBufferedBodyFilter(_ *http.Request, state *base.ResponseState) error {
	if state == nil {
		return nil
	}
	if state.Header == nil {
		state.Header = make(http.Header)
	}
	original := state.Body
	body := original
	bodyChanged := false
	if p.config.Body != nil {
		body = []byte(*p.config.Body)
		bodyChanged = true
	}
	if p.config.BeforeBody != "" || p.config.beforeBodySet {
		body = append([]byte(p.config.BeforeBody), body...)
		bodyChanged = true
	}
	if p.config.AfterBody != "" || p.config.afterBodySet {
		body = append(body, []byte(p.config.AfterBody)...)
		bodyChanged = true
	}
	if bodyChanged && !bytes.Equal(body, original) {
		state.Body = body
		base.InvalidateBodyDerivedHeaders(state.Header)
	}
	return nil
}

func (p *Plugin) AppliesToResponseSource(source apisixctx.ResponseSource) bool {
	switch source {
	case apisixctx.ResponseSourceUpstream, apisixctx.ResponseSourceAPISIX, apisixctx.ResponseSourceEarlyStop:
		return true
	default:
		return false
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			next.ServeHTTP(w, r)
			return
		}
		recorder := base.GetOrCreateTransformResponseWriter(r)
		next.ServeHTTP(recorder, r)

		p.rewrite(recorder)
		recorder.Commit(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rewrite(resp *base.BufferedResponseWriter) {
	original := resp.Body()
	body := original
	bodyChanged := false
	if p.config.Body != nil {
		body = []byte(*p.config.Body)
		bodyChanged = true
	}
	if p.config.BeforeBody != "" || p.config.beforeBodySet {
		body = append([]byte(p.config.BeforeBody), body...)
		bodyChanged = true
	}
	if p.config.AfterBody != "" || p.config.afterBodySet {
		body = append(body, []byte(p.config.AfterBody)...)
		bodyChanged = true
	}

	if bodyChanged && !bytes.Equal(body, original) {
		resp.ReplaceBody(body)
	}

	for field, value := range p.config.Headers {
		resp.Header().Set(field, fmt.Sprint(value))
	}
}
