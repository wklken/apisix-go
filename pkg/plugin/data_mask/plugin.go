package data_mask

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config    Config
	bodyRules []MaskRule
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

	compiledRegex *regexp.Regexp
	pathSegments  []pathSegment
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
	p.bodyRules = p.bodyRules[:0]
	compileRules := func(rules []MaskRule) error {
		for i := range rules {
			rule := &rules[i]
			rule.compiledRegex = nil
			rule.pathSegments = nil
			if rule.Action == "regex" {
				compiled, err := regexp.Compile(rule.Regex)
				if err != nil {
					return fmt.Errorf("invalid regex %q for %s: %w", rule.Regex, rule.Name, err)
				}
				rule.compiledRegex = compiled
			}
			if rule.Type == "body" && rule.BodyFormat == "json" {
				rule.pathSegments = parseJSONPath(rule.Name)
			}
			if rule.Type == "body" {
				p.bodyRules = append(p.bodyRules, *rule)
			}
		}
		return nil
	}
	return compileRules(p.config.Request)
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return next
}

func (p *Plugin) LogCapturePolicy() base.LogCapturePolicy {
	if len(p.bodyRules) == 0 {
		return base.LogCapturePolicy{}
	}
	return base.LogCapturePolicy{RequestBodyBytes: min(p.config.MaxBodySize, base.MAX_REQ_BODY)}
}

func (p *Plugin) SanitizeLogSnapshot(snapshot *base.LogSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("cannot sanitize a nil log snapshot")
	}
	queryChanged := false
	for _, rule := range p.config.Request {
		switch rule.Type {
		case "query":
			queryChanged = maskSnapshotQuery(snapshot, rule) || queryChanged
		case "header":
			maskSnapshotHeader(snapshot, rule)
		}
	}
	if queryChanged {
		if err := updateSnapshotRequestTargets(snapshot); err != nil {
			return err
		}
	}
	if len(p.bodyRules) == 0 {
		return nil
	}
	if len(snapshot.Request.Body) == 0 {
		return nil
	}
	if snapshot.Request.BodyTruncated || len(snapshot.Request.Body) > p.config.MaxBodySize {
		snapshot.Request.Body = nil
		snapshot.Request.BodyTruncated = true
		return fmt.Errorf("request body is incomplete for data masking")
	}
	body, _, err := p.maskBodyRules(snapshot.Request.Body, p.bodyRules)
	if err != nil {
		snapshot.Request.Body = nil
		snapshot.Request.BodyTruncated = true
		return fmt.Errorf("mask request body: %w", err)
	}
	snapshot.Request.Body = body
	delete(snapshot.Request.RequestVars, "$request_body")
	return nil
}

func maskSnapshotQuery(snapshot *base.LogSnapshot, rule MaskRule) bool {
	if snapshot.Request.Query == nil {
		snapshot.Request.Query = make(url.Values)
	}
	return maskValues(snapshot.Request.Query, rule)
}

func maskSnapshotHeader(snapshot *base.LogSnapshot, rule MaskRule) {
	if snapshot.Request.Header == nil {
		snapshot.Request.Header = make(http.Header)
	}
	value := snapshot.Request.Header.Get(rule.Name)
	if value == "" {
		return
	}
	masked := value
	changed := true
	switch rule.Action {
	case "remove":
		snapshot.Request.Header.Del(rule.Name)
		masked = ""
	case "replace":
		snapshot.Request.Header.Set(rule.Name, rule.Value)
		masked = rule.Value
	case "regex":
		var ok bool
		masked, ok = maskString(value, rule)
		if ok {
			snapshot.Request.Header.Set(rule.Name, masked)
		} else {
			changed = false
		}
	default:
		changed = false
	}
	if changed && strings.EqualFold(rule.Name, "X-Request-Id") {
		maskSnapshotRequestID(snapshot, value, masked)
	}
}

func maskSnapshotRequestID(snapshot *base.LogSnapshot, original, masked string) {
	if snapshot.Request.ID == original {
		snapshot.Request.ID = masked
	}
	for _, variables := range []map[string]any{
		snapshot.Request.APISIXVars,
		snapshot.Request.RequestVars,
	} {
		for _, key := range []string{"$request_id", "$http_x_request_id"} {
			value, ok := variables[key].(string)
			if !ok || value != original {
				continue
			}
			if masked == "" {
				delete(variables, key)
			} else {
				variables[key] = masked
			}
		}
	}
}

func updateSnapshotRequestTargets(snapshot *base.LogSnapshot) error {
	encoded := snapshot.Request.Query.Encode()
	if snapshot.Request.URI != "" {
		parsed, err := url.ParseRequestURI(snapshot.Request.URI)
		if err != nil {
			snapshot.Request.URI = ""
			snapshot.Request.URL = ""
			return fmt.Errorf("parse detached request URI: %w", err)
		}
		parsed.RawQuery = encoded
		snapshot.Request.URI = parsed.RequestURI()
	}
	if snapshot.Request.URL != "" {
		parsed, err := url.Parse(snapshot.Request.URL)
		if err != nil {
			snapshot.Request.URI = ""
			snapshot.Request.URL = ""
			return fmt.Errorf("parse detached request URL: %w", err)
		}
		parsed.RawQuery = encoded
		snapshot.Request.URL = parsed.String()
	}
	return nil
}

func (p *Plugin) maskBodyRules(body []byte, rules []MaskRule) ([]byte, bool, error) {
	masked := false
	for start := 0; start < len(rules); {
		format := rules[start].BodyFormat
		end := start + 1
		for end < len(rules) && rules[end].BodyFormat == format {
			end++
		}

		switch format {
		case "urlencoded":
			values, err := parseURLValues(string(body), *p.config.MaxReqPostArgs)
			if err != nil {
				return nil, false, err
			}
			groupMasked := false
			for _, rule := range rules[start:end] {
				groupMasked = maskValues(values, rule) || groupMasked
			}
			if groupMasked {
				body = []byte(values.Encode())
				masked = true
			}
		case "json":
			var obj any
			if err := json.Unmarshal(body, &obj); err != nil {
				return nil, false, err
			}
			groupMasked := false
			for _, rule := range rules[start:end] {
				groupMasked = maskJSONPath(obj, rule) || groupMasked
			}
			if groupMasked {
				encoded, err := json.Marshal(obj)
				if err != nil {
					return nil, false, err
				}
				body = encoded
				masked = true
			}
		}
		start = end
	}
	return body, masked, nil
}

func parseURLValues(raw string, maxArgs int) (url.Values, error) {
	full, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	if maxArgs <= 0 {
		return full, nil
	}

	values := url.Values{}
	parsed := 0
	for pair := range strings.SplitSeq(raw, "&") {
		if pair == "" {
			continue
		}
		if parsed >= maxArgs {
			return nil, fmt.Errorf("urlencoded body exceeds max_req_post_args %d", maxArgs)
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
	segments := rule.pathSegments
	if len(segments) == 0 {
		segments = parseJSONPath(rule.Name)
	}
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
		segment := segments[0]
		segment.recursive = false
		remaining := make([]pathSegment, len(segments))
		copy(remaining, segments)
		remaining[0] = segment
		return maskJSONRecursive(node, remaining, rule)
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
	if segment.each {
		items, ok := value.([]any)
		if !ok {
			return false
		}
		masked := false
		for index, item := range items {
			if len(segments) == 1 {
				if maskJSONArrayElement(items, index, rule) {
					masked = true
				}
			} else if maskJSONNode(item, segments[1:], rule) {
				masked = true
			}
		}
		return masked
	}
	if segment.hasIndex {
		items, ok := value.([]any)
		if !ok || segment.index < 0 || segment.index >= len(items) {
			return false
		}
		if len(segments) == 1 {
			return maskJSONArrayElement(items, segment.index, rule)
		}
		return maskJSONNode(items[segment.index], segments[1:], rule)
	}
	if len(segments) == 1 {
		return maskJSONField(object, segment.name, rule)
	}
	return maskJSONNode(value, segments[1:], rule)
}

func maskJSONRecursive(node any, remaining []pathSegment, rule MaskRule) bool {
	masked := false
	switch typed := node.(type) {
	case map[string]any:
		if maskJSONNode(typed, remaining, rule) {
			masked = true
		}
		for _, value := range typed {
			if maskJSONRecursive(value, remaining, rule) {
				masked = true
			}
		}
	case []any:
		if remaining[0].name == "" && maskJSONNode(typed, remaining, rule) {
			masked = true
		}
		for _, value := range typed {
			if maskJSONRecursive(value, remaining, rule) {
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
	re := rule.compiledRegex
	if re == nil {
		var err error
		re, err = regexp.Compile(rule.Regex)
		if err != nil {
			return value, false
		}
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

func parseJSONPath(path string) []pathSegment {
	path = strings.TrimSpace(path)
	recursive := false
	if after, ok := strings.CutPrefix(path, "$.."); ok {
		path = after
		recursive = true
	} else if after, ok := strings.CutPrefix(path, "$"); ok {
		path = strings.TrimPrefix(after, ".")
	}
	if path == "" {
		return nil
	}

	segments := make([]pathSegment, 0, strings.Count(path, ".")+1)
	for position := 0; position < len(path); {
		if path[position] == '.' {
			position++
			if position == len(path) || path[position] == '.' {
				return nil
			}
			continue
		}
		if path[position] != '[' {
			end := position
			for end < len(path) && path[end] != '.' && path[end] != '[' {
				end++
			}
			segment, ok := parsePathSegment(path[position:end])
			if !ok {
				return nil
			}
			segments = append(segments, segment)
			position = end
			continue
		}

		close := strings.IndexByte(path[position+1:], ']')
		if close < 0 {
			return nil
		}
		close += position + 1
		content := path[position+1 : close]
		if len(content) >= 2 && (content[0] == '\'' || content[0] == '"') && content[len(content)-1] == content[0] {
			segments = append(segments, pathSegment{name: content[1 : len(content)-1]})
		} else {
			if len(segments) == 0 || (position > 0 && path[position-1] == '.') {
				segments = append(segments, pathSegment{})
			}
			segment := &segments[len(segments)-1]
			if content == "*" {
				segment.each = true
			} else {
				index, err := strconv.Atoi(content)
				if err != nil {
					return nil
				}
				segment.hasIndex = true
				segment.index = index
			}
		}
		position = close + 1
	}
	if len(segments) == 0 {
		return nil
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
