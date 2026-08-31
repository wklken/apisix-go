package acl

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/ohler55/ojg/jp"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
	path   jp.Expr
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
        ".*": {"type": "array", "minItems": 1, "items": {"type": "string"}}
      }
    },
    "deny_labels": {
      "type": "object",
      "minProperties": 1,
      "patternProperties": {
        ".*": {"type": "array", "minItems": 1, "items": {"type": "string"}}
      }
    },
    "external_user_label_field": {
      "type": "string",
      "minLength": 1,
      "default": "groups"
    },
    "external_user_label_field_key": {"type": "string", "minLength": 1},
    "external_user_label_field_parser": {
      "type": "string",
      "enum": ["segmented_text", "json", "table"]
    },
    "external_user_label_field_separator": {"type": "string", "minLength": 1},
    "rejected_code": {"type": "integer", "minimum": 200, "default": 403},
    "rejected_msg": {"type": "string"}
  },
  "anyOf": [
    {"required": ["allow_labels"]},
    {"required": ["deny_labels"]}
  ],
  "allOf": [
    {
      "if": {
        "required": ["external_user_label_field_parser"],
        "properties": {
          "external_user_label_field_parser": {"const": "segmented_text"}
        }
      },
      "then": {"required": ["external_user_label_field_separator"]}
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
	path, err := jp.ParseString(p.config.ExternalUserLabelField)
	if err != nil {
		return fmt.Errorf("invalid external_user_label_field %q: %w", p.config.ExternalUserLabelField, err)
	}
	p.path = path

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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labels, authenticated := consumerLabels(r)
		parser := ""
		separator := ""
		if !authenticated {
			labels, authenticated = p.externalUserLabels(r)
			parser = p.config.ExternalUserLabelFieldParser
			separator = p.config.ExternalUserLabelFieldSeparator
		}
		if !authenticated {
			writePluginError(w, util.BuildMessageResponse("Missing authentication."), http.StatusUnauthorized)
			return
		}

		if p.config.DenyLabels != nil && containsLabelWithParser(p.config.DenyLabels, labels, parser, separator) {
			writePluginError(w, p.config.rejectBody, p.config.RejectedCode)
			return
		}
		if p.config.AllowLabels != nil && !containsLabelWithParser(p.config.AllowLabels, labels, parser, separator) {
			writePluginError(w, p.config.rejectBody, p.config.RejectedCode)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writePluginError(w http.ResponseWriter, body string, status int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, body)
}

func (p *Plugin) externalUserLabels(r *http.Request) (map[string]any, bool) {
	user, ok := ctx.GetApisixVars(r)["$external_user"]
	if !ok || user == nil {
		return nil, false
	}
	if value, isBoolean := user.(bool); isBoolean && !value {
		return nil, false
	}
	value, _ := p.path.FirstFound(user)
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
		value, ok := labels[key]
		if ok && value != nil && containsValueWithParser(wantValues, value, parser, separator) {
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

var labelSeparatorRegexCache sync.Map // separator -> *regexp.Regexp

func labelSeparatorRegex(separator string) *regexp.Regexp {
	if cached, ok := labelSeparatorRegexCache.Load(separator); ok {
		return cached.(*regexp.Regexp)
	}
	re, err := regexp.Compile(`\s*(?:` + separator + `)\s*`)
	if err != nil {
		return nil
	}
	actual, _ := labelSeparatorRegexCache.LoadOrStore(separator, re)
	return actual.(*regexp.Regexp)
}

func extractValuesWithParser(value any, parser, separator string) []string {
	switch parser {
	case "segmented_text":
		text, ok := value.(string)
		if !ok {
			return nil
		}
		re := labelSeparatorRegex(separator)
		if re == nil {
			logger.Warnf("failed to split labels [%s], err: invalid separator %q", text, separator)
			return nil
		}
		return re.Split(text, -1)
	case "json":
		text, ok := value.(string)
		if !ok {
			logger.Warnf("the parser is specified as json array, but the value type is not string")
			return nil
		}
		if !strings.HasPrefix(text, "[") {
			logger.Warnf("the parser is specified as json array, but the value do not has prefix '['")
			return nil
		}
		var decoded []any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			logger.Warnf("failed to decode labels [%s] as array, err: %v", text, err)
			return nil
		}
		return stringValues(decoded)
	case "table":
		values, ok := tableValues(value)
		if !ok {
			logger.Warnf("the parser is specified as table, but the type of value is not table: %s", luaTypeName(value))
			return nil
		}
		return values
	default:
		return extractValuesWithoutParser(value)
	}
}

func extractValuesWithoutParser(value any) []string {
	if values, ok := tableValues(value); ok {
		return values
	}
	text, ok := value.(string)
	if !ok {
		logger.Errorf("unsupported type of label value: %s", luaTypeName(value))
		return nil
	}
	if strings.HasPrefix(text, "[") {
		return extractValuesWithParser(text, "json", "")
	}
	if strings.Contains(text, ",") {
		return extractValuesWithParser(text, "segmented_text", ",")
	}
	logger.Infof("the string value can not parsed by json or segmented_text")
	return []string{text}
}

func tableValues(value any) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return values, true
	case []any:
		return stringValues(values), true
	default:
		return nil, false
	}
}

func stringValues(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func luaTypeName(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case nil:
		return "nil"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return "number"
	case map[string]any, []any, []string:
		return "table"
	default:
		return fmt.Sprintf("%T", value)
	}
}
