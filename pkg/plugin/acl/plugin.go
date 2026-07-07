package acl

import (
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 2410
	name     = "acl"
)

const schema = `
{
  "type": "object",
  "properties": {
    "allow_labels": {
      "type": "object",
      "minProperties": 1,
      "patternProperties": {
        ".*": {
          "type": "array",
          "minItems": 1,
          "items": {
            "type": "string"
          }
        }
      }
    },
    "deny_labels": {
      "type": "object",
      "minProperties": 1,
      "patternProperties": {
        ".*": {
          "type": "array",
          "minItems": 1,
          "items": {
            "type": "string"
          }
        }
      }
    },
    "rejected_code": {
      "type": "integer",
      "minimum": 200,
      "default": 403
    },
    "rejected_msg": {
      "type": "string"
    }
  },
  "anyOf": [
    {
      "required": ["allow_labels"]
    },
    {
      "required": ["deny_labels"]
    }
  ]
}
`

type Config struct {
	AllowLabels  map[string][]string `json:"allow_labels,omitempty"`
	DenyLabels   map[string][]string `json:"deny_labels,omitempty"`
	RejectedCode int                 `json:"rejected_code,omitempty"`
	RejectedMsg  string              `json:"rejected_msg,omitempty"`

	rejectBody string
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = http.StatusForbidden
	}

	rejectedMsg := p.config.RejectedMsg
	if rejectedMsg == "" {
		rejectedMsg = "The consumer is forbidden."
	}
	p.config.rejectBody = util.BuildMessageResponse(rejectedMsg)

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		consumer, ok := ctx.GetApisixVar(r, "$consumer").(resource.Consumer)
		if !ok || consumer.Username == "" {
			http.Error(w, util.BuildMessageResponse("Missing authentication."), http.StatusUnauthorized)
			return
		}

		labels := consumer.Labels
		if p.config.DenyLabels != nil && containsLabel(p.config.DenyLabels, labels) {
			http.Error(w, p.config.rejectBody, p.config.RejectedCode)
			return
		}

		if p.config.AllowLabels != nil && !containsLabel(p.config.AllowLabels, labels) {
			http.Error(w, p.config.rejectBody, p.config.RejectedCode)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func containsLabel(wantLabels map[string][]string, labels map[string]any) bool {
	if labels == nil {
		return false
	}

	for key, wantValues := range wantLabels {
		if containsValue(wantValues, labels[key]) {
			return true
		}
	}
	return false
}

func containsValue(wantValues []string, value any) bool {
	values := extractValues(value)
	for _, want := range wantValues {
		for _, got := range values {
			if want == got {
				return true
			}
		}
	}
	return false
}

func extractValues(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		values := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				values = append(values, s)
			}
		}
		return values
	case string:
		return extractStringValues(v)
	default:
		return nil
	}
}

func extractStringValues(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	if strings.HasPrefix(value, "[") {
		var values []string
		if err := json.Unmarshal([]byte(value), &values); err == nil {
			return values
		}
	}

	if strings.Contains(value, ",") {
		parts := strings.Split(value, ",")
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				values = append(values, part)
			}
		}
		return values
	}

	return []string{value}
}
