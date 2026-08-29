package data_mask

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

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
				segments, err := parseJSONPath(rule.Name)
				if err != nil {
					p.bodyRules = p.bodyRules[:0]
					return fmt.Errorf("invalid JSONPath for rule %q: %w", rule.Name, err)
				}
				rule.pathSegments = segments
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
	encoded := encodeSnapshotQuery(snapshot.Request.Query)
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

// encodeSnapshotQuery matches APISIX's logged request-target representation for
// masking placeholders. url.Values.Encode correctly escapes the remaining
// query data, but it percent-encodes '*' even though APISIX leaves the masking
// marker visible in $request_uri and $request_line.
func encodeSnapshotQuery(query url.Values) string {
	return strings.ReplaceAll(query.Encode(), "%2A", "*")
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
		var err error
		segments, err = parseJSONPath(rule.Name)
		if err != nil {
			return false
		}
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
	masked := false
	switch typed := node.(type) {
	case map[string]any:
		for _, field := range selectedObjectFields(typed, segment) {
			if len(segments) == 1 {
				masked = maskJSONField(typed, field, rule) || masked
				continue
			}
			if maskJSONNode(typed[field], segments[1:], rule) {
				masked = true
			}
		}
	case []any:
		for _, index := range selectedArrayIndexes(len(typed), segment) {
			if len(segments) == 1 {
				masked = maskJSONArrayElement(typed, index, rule) || masked
				continue
			}
			if maskJSONNode(typed[index], segments[1:], rule) {
				masked = true
			}
		}
	}
	return masked
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
		if maskJSONNode(typed, remaining, rule) {
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

func selectedObjectFields(object map[string]any, segment pathSegment) []string {
	if segment.wildcard {
		fields := make([]string, 0, len(object))
		for field := range object {
			fields = append(fields, field)
		}
		return fields
	}
	fields := make([]string, 0, len(segment.fields))
	for _, field := range segment.fields {
		if _, ok := object[field]; ok {
			fields = append(fields, field)
		}
	}
	return fields
}

func selectedArrayIndexes(length int, segment pathSegment) []int {
	if segment.wildcard {
		indexes := make([]int, length)
		for i := range length {
			indexes[i] = i
		}
		return indexes
	}
	if segment.slice != nil {
		return segment.slice.indexes(length)
	}
	indexes := make([]int, 0, len(segment.indexes))
	seen := make(map[int]struct{}, len(segment.indexes))
	for _, index := range segment.indexes {
		if index < 0 {
			index += length
		}
		if index < 0 || index >= length {
			continue
		}
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		indexes = append(indexes, index)
	}
	return indexes
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
	fields    []string
	indexes   []int
	wildcard  bool
	slice     *pathSlice
	recursive bool
}

type pathSlice struct {
	start *int
	end   *int
	step  int
}

func (slice pathSlice) indexes(length int) []int {
	if length == 0 {
		return nil
	}
	if slice.step > 0 {
		start := normalizePositiveSliceBound(slice.start, 0, length)
		end := normalizePositiveSliceBound(slice.end, length, length)
		if start >= end {
			return nil
		}
		count := 1 + (end-1-start)/slice.step
		indexes := make([]int, 0, count)
		for index, remaining := start, count; remaining > 0; remaining-- {
			indexes = append(indexes, index)
			if remaining > 1 {
				index += slice.step
			}
		}
		return indexes
	}

	start := normalizeNegativeSliceBound(slice.start, length-1, length)
	end := normalizeNegativeSliceBound(slice.end, -1, length)
	indexes := make([]int, 0)
	for index := start; index > end; index += slice.step {
		indexes = append(indexes, index)
	}
	return indexes
}

func normalizePositiveSliceBound(bound *int, fallback, length int) int {
	if bound == nil {
		return fallback
	}
	value := *bound
	if value < 0 {
		value += length
	}
	return min(max(value, 0), length)
}

func normalizeNegativeSliceBound(bound *int, fallback, length int) int {
	if bound == nil {
		return fallback
	}
	value := *bound
	if value < 0 {
		value += length
	}
	return min(max(value, -1), length-1)
}

func parseJSONPath(path string) ([]pathSegment, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("path is empty")
	}

	position := 0
	nextRecursive := false
	switch path[0] {
	case '$':
		position++
		if position == len(path) {
			return nil, fmt.Errorf("root-only path cannot select a value")
		}
		switch path[position] {
		case '[':
		case '.':
			position++
			if position < len(path) && path[position] == '.' {
				nextRecursive = true
				position++
			}
		default:
			return nil, fmt.Errorf("root marker must be followed by '.' or '['")
		}
	case '.':
		position++
	}
	if position == len(path) || path[position] == '.' {
		return nil, fmt.Errorf("path has an empty selector")
	}

	segments := make([]pathSegment, 0, strings.Count(path, ".")+1)
	for position < len(path) {
		segment, next, err := parseJSONPathSegment(path, position)
		if err != nil {
			return nil, err
		}
		segment.recursive = nextRecursive
		nextRecursive = false
		segments = append(segments, segment)
		position = next
		if position == len(path) {
			break
		}
		if path[position] == '[' {
			continue
		}
		if path[position] != '.' {
			return nil, fmt.Errorf("selector must be followed by '.' or '['")
		}
		position++
		if position < len(path) && path[position] == '.' {
			nextRecursive = true
			position++
		}
		if position == len(path) || path[position] == '.' {
			return nil, fmt.Errorf("path has an empty selector")
		}
	}
	return segments, nil
}

func parseJSONPathSegment(path string, position int) (pathSegment, int, error) {
	if path[position] == '[' {
		close, err := findBracketClose(path, position)
		if err != nil {
			return pathSegment{}, 0, err
		}
		segment, err := parseBracketSelector(strings.TrimSpace(path[position+1 : close]))
		return segment, close + 1, err
	}

	end := position
	for end < len(path) && path[end] != '.' && path[end] != '[' {
		end++
	}
	name := path[position:end]
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, `]$,'":?()@`) {
		return pathSegment{}, 0, fmt.Errorf("invalid dot selector")
	}
	if name == "*" {
		return pathSegment{wildcard: true}, end, nil
	}
	return pathSegment{fields: []string{name}}, end, nil
}

func findBracketClose(path string, open int) (int, error) {
	quote := byte(0)
	escaped := false
	for position := open + 1; position < len(path); position++ {
		char := path[position]
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			switch char {
			case '\\':
				escaped = true
			case quote:
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == ']' {
			return position, nil
		}
	}
	return 0, fmt.Errorf("unterminated bracket selector")
}

func parseBracketSelector(content string) (pathSegment, error) {
	if content == "" {
		return pathSegment{}, fmt.Errorf("bracket selector is empty")
	}
	if content == "*" {
		return pathSegment{wildcard: true}, nil
	}
	if content[0] == '\'' || content[0] == '"' {
		fields, err := parseQuotedFieldUnion(content)
		if err != nil {
			return pathSegment{}, err
		}
		return pathSegment{fields: fields}, nil
	}
	if strings.Contains(content, ":") {
		slice, err := parseArraySlice(content)
		if err != nil {
			return pathSegment{}, err
		}
		return pathSegment{slice: &slice}, nil
	}
	if strings.ContainsAny(content, `?'"()@`) {
		return pathSegment{}, fmt.Errorf("unsupported bracket selector")
	}
	parts := strings.Split(content, ",")
	indexes := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return pathSegment{}, fmt.Errorf("array index union has an empty member")
		}
		index, err := strconv.Atoi(part)
		if err != nil {
			return pathSegment{}, fmt.Errorf("array selector is not an integer union")
		}
		indexes = append(indexes, index)
	}
	return pathSegment{indexes: indexes}, nil
}

func parseQuotedFieldUnion(content string) ([]string, error) {
	fields := make([]string, 0, strings.Count(content, ",")+1)
	for position := 0; position < len(content); {
		for position < len(content) && (content[position] == ' ' || content[position] == '\t') {
			position++
		}
		if position == len(content) || (content[position] != '\'' && content[position] != '"') {
			return nil, fmt.Errorf("object field union requires quoted members")
		}
		field, next, err := parseQuotedField(content, position)
		if err != nil {
			return nil, err
		}
		if field == "" {
			return nil, fmt.Errorf("object field union has an empty member")
		}
		fields = append(fields, field)
		position = next
		for position < len(content) && (content[position] == ' ' || content[position] == '\t') {
			position++
		}
		if position == len(content) {
			break
		}
		if content[position] != ',' {
			return nil, fmt.Errorf("object field union members must be comma-separated")
		}
		position++
		if position == len(content) {
			return nil, fmt.Errorf("object field union has an empty member")
		}
	}
	return fields, nil
}

func parseQuotedField(content string, position int) (string, int, error) {
	quote := content[position]
	position++
	var field strings.Builder
	for position < len(content) {
		char := content[position]
		position++
		if char == quote {
			return field.String(), position, nil
		}
		if char != '\\' {
			field.WriteByte(char)
			continue
		}
		if position == len(content) {
			return "", 0, fmt.Errorf("quoted field has an incomplete escape")
		}
		escaped := content[position]
		position++
		switch escaped {
		case '\\', '/', '\'', '"':
			field.WriteByte(escaped)
		case 'b':
			field.WriteByte('\b')
		case 'f':
			field.WriteByte('\f')
		case 'n':
			field.WriteByte('\n')
		case 'r':
			field.WriteByte('\r')
		case 't':
			field.WriteByte('\t')
		case 'u':
			if position+4 > len(content) {
				return "", 0, fmt.Errorf("quoted field has an incomplete unicode escape")
			}
			codepoint, err := strconv.ParseUint(content[position:position+4], 16, 16)
			if err != nil {
				return "", 0, fmt.Errorf("quoted field has an invalid unicode escape")
			}
			position += 4
			first := rune(codepoint)
			switch {
			case 0xD800 <= first && first <= 0xDBFF:
				if position+6 > len(content) || content[position] != '\\' || content[position+1] != 'u' {
					return "", 0, fmt.Errorf("quoted field has an unpaired high surrogate")
				}
				codepoint, err = strconv.ParseUint(content[position+2:position+6], 16, 16)
				if err != nil || codepoint < 0xDC00 || codepoint > 0xDFFF {
					return "", 0, fmt.Errorf("quoted field has an unpaired high surrogate")
				}
				field.WriteRune(utf16.DecodeRune(first, rune(codepoint)))
				position += 6
			case 0xDC00 <= first && first <= 0xDFFF:
				return "", 0, fmt.Errorf("quoted field has an unpaired low surrogate")
			default:
				field.WriteRune(first)
			}
		default:
			return "", 0, fmt.Errorf("quoted field has an unsupported escape")
		}
	}
	return "", 0, fmt.Errorf("quoted field is unterminated")
}

func parseArraySlice(content string) (pathSlice, error) {
	if strings.Contains(content, ",") {
		return pathSlice{}, fmt.Errorf("slice cannot be combined with a union")
	}
	parts := strings.Split(content, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return pathSlice{}, fmt.Errorf("slice requires start:end or start:end:step")
	}
	start, err := parseOptionalInteger(parts[0])
	if err != nil {
		return pathSlice{}, fmt.Errorf("slice start is not an integer")
	}
	end, err := parseOptionalInteger(parts[1])
	if err != nil {
		return pathSlice{}, fmt.Errorf("slice end is not an integer")
	}
	step := 1
	if len(parts) == 3 {
		parsed, err := parseOptionalInteger(parts[2])
		if err != nil || parsed == nil {
			return pathSlice{}, fmt.Errorf("slice step is not an integer")
		}
		step = *parsed
	}
	if step == 0 {
		return pathSlice{}, fmt.Errorf("slice step cannot be zero")
	}
	return pathSlice{start: start, end: end, step: step}, nil
}

func parseOptionalInteger(raw string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
