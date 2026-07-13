package ai_prompt_decorator

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 1070
	name     = "ai-prompt-decorator"
)

const schema = `
{
  "type": "object",
  "properties": {
    "prepend": {
      "type": "array",
      "items": {
        "$ref": "#/$defs/prompt"
      }
    },
    "append": {
      "type": "array",
      "items": {
        "$ref": "#/$defs/prompt"
      }
    }
  },
  "anyOf": [
    {
      "required": ["prepend"]
    },
    {
      "required": ["append"]
    },
    {
      "required": ["append", "prepend"]
    }
  ],
  "$defs": {
    "prompt": {
      "type": "object",
      "properties": {
        "role": {
          "type": "string",
          "enum": ["system", "user", "assistant"]
        },
        "content": {
          "type": "string",
          "minLength": 1
        }
      },
      "required": ["role", "content"]
    }
  }
}
`

type Config struct {
	Prepend []Message `json:"prepend,omitempty"`
	Append  []Message `json:"append,omitempty"`
}

type Message = ai_protocols.Message

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
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		body, err := base.ReadRequestBody(r)
		if err != nil {
			base.WriteJSONMessage(w, http.StatusBadRequest, "could not get body: "+err.Error())
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			base.WriteJSONMessage(w, http.StatusBadRequest, "could not get body: request body is empty")
			return
		}

		var bodyTab map[string]any
		if err := json.Unmarshal(body, &bodyTab); err != nil {
			base.WriteJSONMessage(w, http.StatusBadRequest, "could not parse JSON request body: "+err.Error())
			return
		}

		protocol, err := ai_protocols.Detect(r.URL.Path, bodyTab)
		if err != nil {
			base.WriteJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}
		ai_protocols.PrependMessages(protocol, bodyTab, p.config.Prepend)
		ai_protocols.AppendMessages(protocol, bodyTab, p.config.Append)

		rewritten, err := json.Marshal(bodyTab)
		if err != nil {
			base.WriteJSONMessage(
				w,
				http.StatusInternalServerError,
				"failed to parse modified JSON request body: "+err.Error(),
			)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(rewritten))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rewritten)), nil
		}
		r.ContentLength = int64(len(rewritten))
		r.Header.Set("Content-Length", fmt.Sprint(len(rewritten)))

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
