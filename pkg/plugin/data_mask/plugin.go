package data_mask

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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
        "required": ["type", "name", "action"],
        "allOf": [
          {
            "if": {
              "required": ["type"],
              "properties": {"type": {"const": "body"}}
            },
            "then": {"required": ["body_format"]}
          },
          {
            "if": {
              "required": ["action"],
              "properties": {"action": {"const": "regex"}}
            },
            "then": {"required": ["regex", "value"]}
          },
          {
            "if": {
              "required": ["action"],
              "properties": {"action": {"const": "replace"}}
            },
            "then": {"required": ["value"]}
          }
        ]
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

func (p *Plugin) Config() any {
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
		values, err := parseURLValues(string(body), *p.config.MaxReqPostArgs)
		if err != nil {
			return false, body, err
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

func parseURLValues(raw string, maxArgs int) (url.Values, error) {
	if _, err := url.ParseQuery(raw); err != nil {
		return nil, err
	}
	if maxArgs <= 0 {
		return url.ParseQuery(raw)
	}

	values := url.Values{}
	parsed := 0
	for pair := range strings.SplitSeq(raw, "&") {
		if pair == "" {
			continue
		}
		if parsed >= maxArgs {
			break
		}
		key, value, hasValue := strings.Cut(pair, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			return nil, err
		}
		if hasValue {
			value, err = url.QueryUnescape(value)
			if err != nil {
				return nil, err
			}
		} else {
			value = ""
		}
		values.Add(decodedKey, value)
		parsed++
	}
	return values, nil
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
	if segments[0].recursive {
		return maskJSONRecursive(node, segments, rule)
	}
	segment := segments[0]
	if segment.name == "" {
		items, ok := node.([]any)
		if !ok {
			return false
		}
		if segment.each {
			masked := false
			for index, item := range items {
				if len(segments) == 1 {
					if maskJSONArrayElement(items, index, rule) {
						masked = true
					}
					continue
				}
				if maskJSONNode(item, segments[1:], rule) {
					masked = true
				}
			}
			return masked
		}
		if !segment.hasIndex || segment.index < 0 || segment.index >= len(items) {
			return false
		}
		if len(segments) == 1 {
			return maskJSONArrayElement(items, segment.index, rule)
		}
		return maskJSONNode(items[segment.index], segments[1:], rule)
	}
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
	if segment.hasIndex {
		items, ok := value.([]any)
		if !ok {
			return false
		}
		if segment.index < 0 || segment.index >= len(items) {
			return false
		}
		if len(segments) == 1 {
			return maskJSONArrayElement(items, segment.index, rule)
		}
		return maskJSONNode(items[segment.index], segments[1:], rule)
	}
	return maskJSONNode(value, segments[1:], rule)
}

func maskJSONRecursive(node any, segments []pathSegment, rule MaskRule) bool {
	if len(segments) == 0 {
		return false
	}
	segment := segments[0]
	segment.recursive = false
	remaining := make([]pathSegment, len(segments))
	copy(remaining, segments)
	remaining[0] = segment

	masked := false
	switch typed := node.(type) {
	case map[string]any:
		if value, ok := typed[segment.name]; ok {
			if len(remaining) == 1 {
				if maskJSONField(typed, segment.name, rule) {
					masked = true
				}
			} else if maskJSONNode(value, remaining[1:], rule) {
				masked = true
			}
		}
		for _, value := range typed {
			if maskJSONRecursive(value, segments, rule) {
				masked = true
			}
		}
	case []any:
		for _, value := range typed {
			if maskJSONRecursive(value, segments, rule) {
				masked = true
			}
		}
	}
	return masked
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

func maskJSONArrayElement(items []any, index int, rule MaskRule) bool {
	value := items[index]
	switch rule.Action {
	case "remove":
		items[index] = nil
		return true
	case "replace":
		items[index] = rule.Value
		return true
	case "regex":
		valueString, ok := value.(string)
		if !ok {
			return false
		}
		if masked, ok := maskString(valueString, rule); ok {
			items[index] = masked
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
	match := re.FindStringSubmatchIndex(value)
	if match == nil {
		return value, false
	}
	masked := make([]byte, 0, len(value)+len(rule.Value))
	masked = append(masked, value[:match[0]]...)
	masked = re.ExpandString(masked, rule.Value, value, match)
	masked = append(masked, value[match[1]:]...)
	return string(masked), true
}

type pathSegment struct {
	name      string
	each      bool
	hasIndex  bool
	index     int
	recursive bool
}

var quotedJSONPathSegment = regexp.MustCompile(`\[(?:"([^"]+)"|'([^']+)')\]`)

func parseJSONPath(path string) []pathSegment {
	path = strings.TrimSpace(path)
	path = quotedJSONPathSegment.ReplaceAllStringFunc(path, func(segment string) string {
		matches := quotedJSONPathSegment.FindStringSubmatch(segment)
		if matches[1] != "" {
			return "." + matches[1]
		}
		return "." + matches[2]
	})
	recursive := false
	if after, ok := strings.CutPrefix(path, "$.."); ok {
		path = after
		recursive = true
	} else if after, ok := strings.CutPrefix(path, "$."); ok {
		path = after
	} else if after, ok := strings.CutPrefix(path, "$"); ok {
		path = after
		path = strings.TrimPrefix(path, ".")
	}
	if path == "" {
		return nil
	}
	parts := strings.Split(path, ".")
	segments := make([]pathSegment, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil
		}
		segment, ok := parsePathSegment(part)
		if !ok {
			return nil
		}
		segments = append(segments, segment)
	}
	if recursive {
		segments[0].recursive = true
	}
	return segments
}

func parsePathSegment(part string) (pathSegment, bool) {
	segment := pathSegment{name: part}
	if before, ok := strings.CutSuffix(part, "[*]"); ok {
		segment.name = before
		segment.each = true
		return segment, segment.name != "" || segment.each
	}
	if !strings.HasSuffix(part, "]") {
		return segment, true
	}
	open := strings.LastIndex(part, "[")
	if open < 0 {
		return pathSegment{}, false
	}
	index, err := strconv.Atoi(part[open+1 : len(part)-1])
	if err != nil {
		return pathSegment{}, false
	}
	segment.name = part[:open]
	segment.hasIndex = true
	segment.index = index
	return segment, segment.name != "" || segment.hasIndex
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
