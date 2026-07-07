package attach_consumer_label

import (
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 2399
	name     = "attach-consumer-label"
)

const schema = `
{
  "type": "object",
  "properties": {
    "headers": {
      "type": "object",
      "additionalProperties": {
        "type": "string",
        "pattern": "^\\$.*"
      },
      "minProperties": 1
    }
  },
  "required": ["headers"]
}
`

type Config struct {
	Headers map[string]string `json:"headers"`
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
		consumer, ok := ctx.GetApisixVar(r, "$consumer").(resource.Consumer)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		for header, labelRef := range p.config.Headers {
			value, ok := labelValue(consumer.Labels, labelRef)
			if !ok {
				continue
			}
			r.Header.Set(header, value)
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func labelValue(labels map[string]any, labelRef string) (string, bool) {
	if labels == nil || !strings.HasPrefix(labelRef, "$") {
		return "", false
	}

	value, ok := labels[strings.TrimPrefix(labelRef, "$")]
	if !ok || value == nil {
		return "", false
	}

	s, ok := value.(string)
	if !ok {
		return "", false
	}

	return s, true
}
