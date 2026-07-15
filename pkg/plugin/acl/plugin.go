package acl

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
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
	if err := validateExternalUserLabelField(p.config.ExternalUserLabelField); err != nil {
		return err
	}
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

func (p *Plugin) Config() any {
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

func containsValueWithParser(wantValues []string, value any, parser, separator string) bool {
	values := extractValuesWithParser(value, parser, separator)
	for _, want := range wantValues {
		if slices.Contains(values, want) {
			return true
		}
	}
	return false
}

func extractValuesWithParser(value any, parser, separator string) []string {
	if matches, ok := value.(externalUserMatches); ok {
		var values []string
		for _, match := range matches {
			values = append(values, extractValuesWithParser(match, parser, separator)...)
		}
		return values
	}
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
		if _, ok := value.([]any); !ok {
			if _, ok := value.([]string); !ok {
				return nil
			}
		}
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

	path = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(path), "$"))
	if strings.HasPrefix(path, "..") {
		segments := pathSegments(strings.TrimPrefix(path, ".."))
		matches := findExternalUserFields(object, segments)
		return externalUserMatches(matches), len(matches) > 0
	}
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return object, true
	}
	if prefix, suffix, ok := strings.Cut(path, ".."); ok {
		current, found := exactExternalUserField(object, pathSegments(prefix))
		if !found {
			return nil, false
		}
		matches := findExternalUserFields(current, pathSegments(suffix))
		return externalUserMatches(matches), len(matches) > 0
	}
	return exactExternalUserField(object, pathSegments(path))
}

func exactExternalUserField(object map[string]any, segments []string) (any, bool) {
	var current any = object
	for _, segment := range segments {
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

func findExternalUserFields(value any, segments []string) []any {
	if len(segments) == 0 {
		return []any{value}
	}

	var matches []any
	switch typed := value.(type) {
	case map[string]any:
		if current, ok := typed[segments[0]]; ok {
			if len(segments) == 1 {
				matches = append(matches, current)
			} else if current, ok := exactExternalUserFieldValue(current, segments[1:]); ok {
				matches = append(matches, current)
			}
		}
		for _, child := range typed {
			matches = append(matches, findExternalUserFields(child, segments)...)
		}
	case []any:
		for _, child := range typed {
			matches = append(matches, findExternalUserFields(child, segments)...)
		}
	}
	return matches
}

func exactExternalUserFieldValue(value any, segments []string) (any, bool) {
	current := value
	for _, segment := range segments {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

type externalUserMatches []any

func pathSegments(path string) []string {
	parts := strings.Split(path, ".")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			segments = append(segments, part)
		}
	}
	return segments
}

func validateExternalUserLabelField(path string) error {
	path = strings.TrimSpace(path)
	if path == "" || !strings.HasPrefix(path, "$") {
		return nil
	}
	if strings.ContainsAny(path, "[]()") || len(pathSegments(strings.ReplaceAll(path, "..", "."))) == 0 {
		return fmt.Errorf("invalid external_user_label_field %q", path)
	}
	return nil
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
