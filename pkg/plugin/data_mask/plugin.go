package data_mask

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 1500
	name     = "data-mask"
)

const schema = `
{
  "type": "object",
  "properties": {
    "request": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "type": {"type": "string", "enum": ["query", "header", "body"]},
          "body_format": {"type": "string", "enum": ["json", "urlencoded"]},
          "name": {"type": "string"},
          "action": {"type": "string", "enum": ["regex", "replace", "remove"]},
          "regex": {"type": "string"},
          "value": {"type": "string"}
        },
        "required": ["type", "name", "action"]
      }
    },
    "max_body_size": {
      "type": "integer",
      "exclusiveMinimum": 0,
      "default": 1048576
    },
    "max_req_post_args": {
      "type": "integer",
      "minimum": 0,
      "default": 100
    }
  }
}
`

type Config struct {
	Request        []MaskRule `json:"request,omitempty"`
	MaxBodySize    int        `json:"max_body_size,omitempty"`
	MaxReqPostArgs *int       `json:"max_req_post_args,omitempty"`
}

type MaskRule struct {
	Type       string `json:"type"`
	BodyFormat string `json:"body_format,omitempty"`
	Name       string `json:"name"`
	Action     string `json:"action"`
	Regex      string `json:"regex,omitempty"`
	Value      string `json:"value,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.MaxBodySize == 0 {
		p.config.MaxBodySize = 1024 * 1024
	}
	if p.config.MaxReqPostArgs == nil {
		n := 100
		p.config.MaxReqPostArgs = &n
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := p.maskRequest(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p *Plugin) maskRequest(r *http.Request) error {
	var body []byte
	bodyLoaded := false
	for _, rule := range p.config.Request {
		switch rule.Type {
		case "query":
			maskQuery(r, rule)
		case "header":
			maskHeader(r, rule)
		case "body":
			if !bodyLoaded {
				var err error
				body, err = readBody(r)
				if err != nil {
					return err
				}
				bodyLoaded = true
			}
			masked, newBody, err := p.maskBody(body, rule)
			if err != nil {
				return err
			}
			if masked {
				body = newBody
			}
		}
	}
	if bodyLoaded {
		setBody(r, body)
	}
	return nil
}

func maskQuery(r *http.Request, rule MaskRule) {
	values := r.URL.Query()
	if !maskValues(values, rule) {
		return
	}
	r.URL.RawQuery = values.Encode()
	if r.RequestURI != "" {
		r.RequestURI = r.URL.RequestURI()
	}
}

func maskHeader(r *http.Request, rule MaskRule) {
	value := r.Header.Get(rule.Name)
	if value == "" {
		return
	}
	switch rule.Action {
	case "remove":
		r.Header.Del(rule.Name)
	case "replace":
		r.Header.Set(rule.Name, rule.Value)
	case "regex":
		if masked, ok := maskString(value, rule); ok {
			r.Header.Set(rule.Name, masked)
		}
	}
}

func (p *Plugin) maskBody(body []byte, rule MaskRule) (bool, []byte, error) {
	switch rule.BodyFormat {
	case "urlencoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return false, body, err
		}
		if *p.config.MaxReqPostArgs > 0 && len(values) > *p.config.MaxReqPostArgs {
			return false, body, nil
		}
		if !maskValues(values, rule) {
			return false, body, nil
		}
		return true, []byte(values.Encode()), nil
	case "json":
		if len(body) > p.config.MaxBodySize {
			return false, body, nil
		}
		var obj any
		if err := json.Unmarshal(body, &obj); err != nil {
			return false, body, err
		}
		if !maskJSONPath(obj, rule) {
			return false, body, nil
		}
		masked, err := json.Marshal(obj)
		if err != nil {
			return false, body, err
		}
		return true, masked, nil
	default:
		return false, body, nil
	}
}

func maskValues(values url.Values, rule MaskRule) bool {
	existing, ok := values[rule.Name]
	if !ok {
		return false
	}
	switch rule.Action {
	case "remove":
		values.Del(rule.Name)
		return true
	case "replace":
		values.Set(rule.Name, rule.Value)
		return true
	case "regex":
		masked := false
		for i, value := range existing {
			if newValue, ok := maskString(value, rule); ok {
				existing[i] = newValue
				masked = true
			}
		}
		if masked {
			values[rule.Name] = existing
		}
		return masked
	default:
		return false
	}
}

func maskJSONPath(root any, rule MaskRule) bool {
	segments := parseJSONPath(rule.Name)
	if len(segments) == 0 {
		return false
	}
	return maskJSONNode(root, segments, rule)
}

func maskJSONNode(node any, segments []pathSegment, rule MaskRule) bool {
	if len(segments) == 0 {
		return false
	}
	segment := segments[0]
	object, ok := node.(map[string]any)
	if !ok {
		return false
	}
	value, exists := object[segment.name]
	if !exists {
		return false
	}
	if len(segments) == 1 {
		return maskJSONField(object, segment.name, rule)
	}
	if segment.each {
		items, ok := value.([]any)
		if !ok {
			return false
		}
		masked := false
		for _, item := range items {
			if maskJSONNode(item, segments[1:], rule) {
				masked = true
			}
		}
		return masked
	}
	return maskJSONNode(value, segments[1:], rule)
}

func maskJSONField(object map[string]any, field string, rule MaskRule) bool {
	value, ok := object[field]
	if !ok {
		return false
	}
	switch rule.Action {
	case "remove":
		delete(object, field)
		return true
	case "replace":
		object[field] = rule.Value
		return true
	case "regex":
		valueString, ok := value.(string)
		if !ok {
			return false
		}
		if masked, ok := maskString(valueString, rule); ok {
			object[field] = masked
			return true
		}
	}
	return false
}

func maskString(value string, rule MaskRule) (string, bool) {
	re, err := regexp.Compile(rule.Regex)
	if err != nil {
		return value, false
	}
	masked := re.ReplaceAllString(value, rule.Value)
	return masked, masked != value
}

type pathSegment struct {
	name string
	each bool
}

func parseJSONPath(path string) []pathSegment {
	if !strings.HasPrefix(path, "$.") {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "$."), ".")
	segments := make([]pathSegment, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil
		}
		segment := pathSegment{name: part}
		if strings.HasSuffix(part, "[*]") {
			segment.name = strings.TrimSuffix(part, "[*]")
			segment.each = true
			if segment.name == "" {
				return nil
			}
		}
		segments = append(segments, segment)
	}
	return segments
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return body, nil
}

func setBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
}
