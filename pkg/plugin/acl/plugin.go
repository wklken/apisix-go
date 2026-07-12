package acl

import (
	"fmt"
	"net/http"
	"regexp"
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
    "external_user_label_field": {
      "type": "string",
      "minLength": 1,
      "default": "groups"
    },
    "external_user_label_field_key": {
      "type": "string",
      "minLength": 1
    },
    "external_user_label_field_parser": {
      "type": "string",
      "enum": ["segmented_text", "json", "table"]
    },
    "external_user_label_field_separator": {
      "type": "string",
      "minLength": 1
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
  ],
  "allOf": [
    {
      "if": {
        "required": ["external_user_label_field_parser"],
        "properties": {
          "external_user_label_field_parser": {"const": "segmented_text"}
        }
      },
      "then": {
        "required": ["external_user_label_field_separator"]
      }
    }
  ]
}
`

type Config struct {
	AllowLabels                     map[string][]string `json:"allow_labels,omitempty"`
	DenyLabels                      map[string][]string `json:"deny_labels,omitempty"`
	ExternalUserLabelField          string              `json:"external_user_label_field,omitempty"`
	ExternalUserLabelFieldKey       string              `json:"external_user_label_field_key,omitempty"`
	ExternalUserLabelFieldParser    string              `json:"external_user_label_field_parser,omitempty"`
	ExternalUserLabelFieldSeparator string              `json:"external_user_label_field_separator,omitempty"`
	RejectedCode                    int                 `json:"rejected_code,omitempty"`
	RejectedMsg                     string              `json:"rejected_msg,omitempty"`

	rejectBody string
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.ExternalUserLabelField == "" {
		p.config.ExternalUserLabelField = "groups"
	}
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
		labels, authenticated := consumerLabels(r)
		parser := ""
		separator := ""
		if !authenticated {
			labels, authenticated = p.externalUserLabels(r)
			parser = p.config.ExternalUserLabelFieldParser
			separator = p.config.ExternalUserLabelFieldSeparator
		}
		if !authenticated {
			http.Error(w, util.BuildMessageResponse("Missing authentication."), http.StatusUnauthorized)
			return
		}

		if p.config.DenyLabels != nil && containsLabelWithParser(p.config.DenyLabels, labels, parser, separator) {
			http.Error(w, p.config.rejectBody, p.config.RejectedCode)
			return
		}

		if p.config.AllowLabels != nil && !containsLabelWithParser(p.config.AllowLabels, labels, parser, separator) {
			http.Error(w, p.config.rejectBody, p.config.RejectedCode)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) externalUserLabels(r *http.Request) (map[string]any, bool) {
	user := ctx.GetApisixVar(r, "$external_user")
	if _, ok := externalUserObject(user); !ok {
		return nil, false
	}
	value, _ := externalUserField(user, p.config.ExternalUserLabelField)

	key := p.config.ExternalUserLabelFieldKey
	if key == "" {
		key = p.config.ExternalUserLabelField
	}
	return map[string]any{key: value}, true
}

func consumerLabels(r *http.Request) (map[string]any, bool) {
	consumer, ok := ctx.GetApisixVar(r, "$consumer").(resource.Consumer)
	if ok && consumer.Username != "" {
		return consumer.Labels, true
	}

	return nil, false
}

func containsLabel(wantLabels map[string][]string, labels map[string]any) bool {
	return containsLabelWithParser(wantLabels, labels, "", "")
}

func containsLabelWithParser(wantLabels map[string][]string, labels map[string]any, parser, separator string) bool {
	if labels == nil {
		return false
	}

	for key, wantValues := range wantLabels {
		if containsValueWithParser(wantValues, labels[key], parser, separator) {
			return true
		}
	}
	return false
}

func containsValue(wantValues []string, value any) bool {
	return containsValueWithParser(wantValues, value, "", "")
}

func containsValueWithParser(wantValues []string, value any, parser, separator string) bool {
	values := extractValuesWithParser(value, parser, separator)
	for _, want := range wantValues {
		for _, got := range values {
			if want == got {
				return true
			}
		}
	}
	return false
}

func extractValuesWithParser(value any, parser, separator string) []string {
	switch parser {
	case "segmented_text":
		text, ok := value.(string)
		if !ok || separator == "" {
			return nil
		}
		re, err := regexp.Compile(`\s*(?:` + separator + `)\s*`)
		if err != nil {
			return nil
		}
		parts := re.Split(text, -1)
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				values = append(values, part)
			}
		}
		return values
	case "json":
		text, ok := value.(string)
		if !ok || !strings.HasPrefix(strings.TrimSpace(text), "[") {
			return nil
		}
		var decoded []any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return nil
		}
		return extractValues(decoded)
	case "table":
		return extractValues(value)
	default:
		return extractValues(value)
	}
}

func externalUserField(user any, path string) (any, bool) {
	object, ok := externalUserObject(user)
	if !ok {
		return nil, false
	}

	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "$..") {
		fieldPath := strings.TrimPrefix(path, "$..")
		if fieldPath == "" {
			return nil, false
		}
		segments := strings.Split(fieldPath, ".")
		for index := range segments {
			segments[index] = strings.TrimSpace(segments[index])
			if segments[index] == "" {
				return nil, false
			}
		}
		return findExternalUserField(object, segments)
	}
	path = strings.TrimPrefix(path, "$")
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return object, true
	}

	var current any = object
	for _, segment := range strings.Split(path, ".") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return nil, false
		}
		values, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = values[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func findExternalUserField(value any, segments []string) (any, bool) {
	if len(segments) == 0 {
		return value, true
	}

	switch typed := value.(type) {
	case map[string]any:
		if current, ok := typed[segments[0]]; ok {
			if len(segments) == 1 {
				return current, true
			}
			if current, ok := findExternalUserField(current, segments[1:]); ok {
				return current, true
			}
		}
		for _, child := range typed {
			if current, ok := findExternalUserField(child, segments); ok {
				return current, true
			}
		}
	case []any:
		for _, child := range typed {
			if current, ok := findExternalUserField(child, segments); ok {
				return current, true
			}
		}
	}
	return nil, false
}

func externalUserObject(user any) (map[string]any, bool) {
	if object, ok := user.(map[string]any); ok {
		return object, true
	}
	if user == nil {
		return nil, false
	}

	raw, err := json.Marshal(user)
	if err != nil {
		return nil, false
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false
	}
	return object, true
}

func extractValues(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		values := make([]string, 0, len(v))
		for _, item := range v {
			if item == nil {
				continue
			}
			if s, ok := item.(string); ok {
				values = append(values, s)
				continue
			}
			values = append(values, fmt.Sprint(item))
		}
		return values
	case string:
		return extractStringValues(v)
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return []string{fmt.Sprint(v)}
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
