package acl

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

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
      "maxLength": 4096,
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
      "maximum": 599,
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
		var labelError error
		if !authenticated {
			labels, authenticated, labelError = p.externalUserLabels(r)
			parser = p.config.ExternalUserLabelFieldParser
			separator = p.config.ExternalUserLabelFieldSeparator
		}
		if labelError != nil {
			writePluginError(w, p.config.rejectBody, p.config.RejectedCode)
			return
		}
		if !authenticated {
			writePluginError(w, util.BuildMessageResponse("Missing authentication."), http.StatusUnauthorized)
			return
		}

		labelBudget := externalUserLabelBudget{}
		if p.config.DenyLabels != nil {
			matched, err := containsLabelWithParser(
				p.config.DenyLabels, labels, parser, separator, &labelBudget,
			)
			if err != nil || matched {
				writePluginError(w, p.config.rejectBody, p.config.RejectedCode)
				return
			}
		}

		if p.config.AllowLabels != nil {
			matched, err := containsLabelWithParser(
				p.config.AllowLabels, labels, parser, separator, &labelBudget,
			)
			if err != nil || !matched {
				writePluginError(w, p.config.rejectBody, p.config.RejectedCode)
				return
			}
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func writePluginError(w http.ResponseWriter, body string, status int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, body)
}

func (p *Plugin) externalUserLabels(r *http.Request) (map[string]any, bool, error) {
	user := ctx.GetApisixVar(r, "$external_user")
	if _, ok := externalUserObject(user); !ok {
		return nil, false, nil
	}
	value, found, err := externalUserField(user, p.config.ExternalUserLabelField)
	if err != nil {
		return nil, true, err
	}

	key := p.config.ExternalUserLabelFieldKey
	if key == "" {
		key = p.config.ExternalUserLabelField
	}
	if !found {
		return map[string]any{}, true, nil
	}
	return map[string]any{key: value}, true, nil
}

func consumerLabels(r *http.Request) (map[string]any, bool) {
	consumer, ok := ctx.GetApisixVar(r, "$consumer").(resource.Consumer)
	if ok && consumer.Username != "" {
		return consumer.Labels, true
	}

	return nil, false
}

func containsLabelWithParser(
	wantLabels map[string][]string,
	labels map[string]any,
	parser string,
	separator string,
	budget *externalUserLabelBudget,
) (bool, error) {
	if labels == nil {
		return false, nil
	}

	for key, wantValues := range wantLabels {
		value, ok := labels[key]
		if !ok {
			continue
		}
		matched, err := containsValueWithParser(wantValues, value, parser, separator, budget)
		if err != nil || matched {
			return matched, err
		}
	}
	return false, nil
}

func containsValueWithParser(
	wantValues []string,
	value any,
	parser string,
	separator string,
	budget *externalUserLabelBudget,
) (bool, error) {
	values, err := extractValuesWithParser(value, parser, separator, budget)
	if err != nil {
		return false, err
	}
	for _, want := range wantValues {
		if slices.Contains(values, want) {
			return true, nil
		}
	}
	return false, nil
}

// labelSeparatorRegexCache caches compiled label separators; an invalid
// separator is not cached so the failure keeps surfacing per request.
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

const (
	maxExternalUserJSONPathBytes = 4096
	maxExternalUserLabelValues   = 4096
	maxExternalUserLabelBytes    = 256 * 1024
)

type externalUserLabelBudget struct {
	values int
	bytes  int
}

func (budget *externalUserLabelBudget) reserveBytes(size int) error {
	if budget == nil || size < 0 || size > maxExternalUserLabelBytes-budget.bytes {
		return fmt.Errorf("label byte budget exceeded")
	}
	budget.bytes += size
	return nil
}

func (budget *externalUserLabelBudget) appendValue(
	values []string, value string, bytesReserved bool,
) ([]string, error) {
	if err := budget.reserveValue(); err != nil {
		return nil, err
	}
	if !bytesReserved {
		if err := budget.reserveBytes(len(value)); err != nil {
			return nil, err
		}
	}
	return append(values, value), nil
}

func (budget *externalUserLabelBudget) reserveValue() error {
	if budget == nil || budget.values >= maxExternalUserLabelValues {
		return fmt.Errorf("label value budget exceeded")
	}
	budget.values++
	return nil
}

func (budget *externalUserLabelBudget) remainingValues() int {
	if budget == nil || budget.values >= maxExternalUserLabelValues {
		return 0
	}
	return maxExternalUserLabelValues - budget.values
}

func extractValuesWithParser(
	value any, parser, separator string, budget *externalUserLabelBudget,
) ([]string, error) {
	if matches, ok := value.(externalUserMatches); ok {
		if len(matches) <= 1 {
			if len(matches) == 0 {
				return nil, nil
			}
			return extractValuesWithParser(matches[0], parser, separator, budget)
		}
		var values []string
		for _, match := range matches {
			matchParser := parser
			if _, stringMatch := match.(string); !stringMatch {
				// APISIX after 3.17 applies a configured parser only to string
				// query results. Tables and scalars retain type-aware handling.
				matchParser = ""
			}
			extracted, err := extractValuesWithParser(match, matchParser, separator, budget)
			if err != nil {
				return nil, err
			}
			values = append(values, extracted...)
		}
		return values, nil
	}
	if value == nil {
		return nil, budget.reserveValue()
	}
	switch parser {
	case "segmented_text":
		text, ok := value.(string)
		if !ok {
			return nil, nil
		}
		if separator == "" {
			return nil, nil
		}
		if err := budget.reserveBytes(len(text)); err != nil {
			return nil, err
		}
		re := labelSeparatorRegex(separator)
		if re == nil {
			logger.Warnf("failed to split labels [%s], err: invalid separator %q", text, separator)
			return nil, nil
		}
		remaining := budget.remainingValues()
		if remaining == 0 || len(re.FindAllStringIndex(text, remaining)) >= remaining {
			return nil, fmt.Errorf("label value budget exceeded")
		}
		parts := re.Split(text, -1)
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				var err error
				values, err = budget.appendValue(values, part, true)
				if err != nil {
					return nil, err
				}
			}
		}
		return values, nil
	case "json":
		text, ok := value.(string)
		if !ok {
			logger.Warnf("the parser is specified as json array, but the value type is not string")
			return nil, nil
		}
		if err := budget.reserveBytes(len(text)); err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(text)
		if !strings.HasPrefix(trimmed, "[") {
			logger.Warnf("the parser is specified as json array, but the value do not has prefix '['")
			return nil, nil
		}
		var decoded []any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			logger.Warnf("failed to decode labels [%s] as array, err: %v", text, err)
			return nil, nil
		}
		return extractValues(decoded, budget, true)
	case "table":
		if _, ok := value.([]any); !ok {
			if _, ok := value.([]string); !ok {
				logger.Warnf(
					"the parser is specified as table, but the type of value is not table: %s",
					luaTypeName(value),
				)
				return nil, nil
			}
		}
		return extractValues(value, budget, false)
	default:
		return extractValues(value, budget, false)
	}
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
	case map[string]any:
		return "table"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func externalUserField(user any, path string) (any, bool, error) {
	object, ok := externalUserObject(user)
	if !ok {
		return nil, false, nil
	}

	steps, err := parseExternalUserJSONPath(path)
	if err != nil {
		return nil, false, err
	}
	if len(steps) == 0 {
		return object, true, nil
	}

	nodes := []externalUserPathNode{{value: object, path: []string{"$"}}}
	budget := externalUserTraversalBudget{}
	multiple := false
	for _, step := range steps {
		var selected []externalUserPathNode
		for _, node := range nodes {
			var matches []externalUserPathNode
			if step.recursive {
				matches, err = findExternalUserNodes(node, step.selector, &budget, 0)
			} else {
				matches, err = selectExternalUserNodes(node, step.selector, &budget)
			}
			if err != nil {
				return nil, false, err
			}
			selected = append(selected, matches...)
		}
		selected = deduplicateExternalUserPathNodes(selected)
		if len(selected) > maxExternalUserJSONPathResults {
			return nil, false, fmt.Errorf("JSONPath result budget exceeded")
		}
		if len(selected) == 0 {
			return nil, false, nil
		}
		nodes = selected
		multiple = multiple || step.recursive || step.selector.canMatchMultiple()
	}
	sort.SliceStable(nodes, func(left, right int) bool {
		return strings.Join(nodes[left].path, ".") < strings.Join(nodes[right].path, ".")
	})

	if multiple || len(nodes) > 1 {
		values := make(externalUserMatches, 0, len(nodes))
		for _, node := range nodes {
			values = append(values, node.value)
		}
		return values, true, nil
	}
	return nodes[0].value, true, nil
}

type externalUserPathSelectorKind uint8

const (
	externalUserPathKey externalUserPathSelectorKind = iota
	externalUserPathIndex
	externalUserPathWildcard
	externalUserPathUnion
	externalUserPathFilter
	externalUserPathScript
)

const (
	maxExternalUserJSONPathSteps           = 64
	maxExternalUserJSONPathRecursiveDepth  = 64
	maxExternalUserJSONPathVisitedNodes    = 4096
	maxExternalUserJSONPathResults         = 1024
	maxExternalUserScriptComponentVisits   = 4096
	maxExternalUserScriptComponentBytes    = 256 * 1024
	maxExternalUserJSONPathSelectorTerms   = 256
	maxExternalUserJSONPathExpressionNodes = 256
	maxExternalUserJSONPathExpressionDepth = 64
)

type externalUserPathSelector struct {
	kind       externalUserPathSelectorKind
	component  string
	union      []externalUserPathUnionMember
	expression *externalUserExpression
}

type externalUserPathStep struct {
	recursive bool
	selector  externalUserPathSelector
}

type externalUserPathUnionMember struct {
	component *string
	slice     *externalUserPathSlice
}

type externalUserPathSlice struct {
	start *float64
	end   *float64
	step  float64
}

type externalUserPathNode struct {
	value any
	path  []string
}

type externalUserPathChild struct {
	component string
	value     any
}

type externalUserTraversalBudget struct {
	visitedNodes          int
	scriptComponentVisits int
	scriptComponentBytes  int
}

func (selector externalUserPathSelector) canMatchMultiple() bool {
	return selector.kind == externalUserPathWildcard ||
		selector.kind == externalUserPathUnion ||
		selector.kind == externalUserPathFilter ||
		selector.kind == externalUserPathScript
}

func selectExternalUserNodes(
	node externalUserPathNode,
	selector externalUserPathSelector,
	budget *externalUserTraversalBudget,
) ([]externalUserPathNode, error) {
	children, err := budget.externalUserPathChildren(node.value)
	if err != nil {
		return nil, err
	}
	return selectExternalUserPathChildren(node, selector, children, budget)
}

func selectExternalUserPathChildren(
	node externalUserPathNode,
	selector externalUserPathSelector,
	children []externalUserPathChild,
	budget *externalUserTraversalBudget,
) ([]externalUserPathNode, error) {
	var scriptComponents map[string]struct{}
	if selector.kind == externalUserPathScript {
		value, err := evaluateExternalUserExpression(selector.expression, node.value)
		if err == errExternalUserExpressionNoValue {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		scriptComponents, err = budget.externalUserScriptComponentSet(value)
		if err != nil {
			return nil, err
		}
	}
	selected := make([]externalUserPathNode, 0, len(children))
	for _, child := range children {
		matched := false
		var err error
		if scriptComponents != nil {
			_, matched = scriptComponents[child.component]
		} else {
			matched, err = externalUserSelectorMatches(selector, node.value, child)
		}
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		path := make([]string, len(node.path)+1)
		copy(path, node.path)
		path[len(node.path)] = child.component
		selected = append(selected, externalUserPathNode{value: child.value, path: path})
	}
	return selected, nil
}

func (budget *externalUserTraversalBudget) externalUserPathChildren(value any) ([]externalUserPathChild, error) {
	count := externalUserPathChildCount(value)
	if count > maxExternalUserJSONPathVisitedNodes-budget.visitedNodes {
		return nil, fmt.Errorf("JSONPath visited-node budget exceeded")
	}
	budget.visitedNodes += count
	return externalUserPathChildren(value), nil
}

func externalUserPathChildCount(value any) int {
	switch current := value.(type) {
	case map[string]any:
		return len(current)
	case []any:
		return len(current)
	case []string:
		return len(current)
	default:
		return 0
	}
}

func externalUserPathChildren(value any) []externalUserPathChild {
	switch current := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		children := make([]externalUserPathChild, 0, len(keys))
		for _, key := range keys {
			children = append(children, externalUserPathChild{component: key, value: current[key]})
		}
		return children
	case []any:
		children := make([]externalUserPathChild, 0, len(current))
		for index, child := range current {
			children = append(children, externalUserPathChild{component: strconv.Itoa(index), value: child})
		}
		return children
	case []string:
		children := make([]externalUserPathChild, 0, len(current))
		for index, child := range current {
			children = append(children, externalUserPathChild{component: strconv.Itoa(index), value: child})
		}
		return children
	default:
		return nil
	}
}

func externalUserSelectorMatches(
	selector externalUserPathSelector,
	parent any,
	child externalUserPathChild,
) (bool, error) {
	switch selector.kind {
	case externalUserPathKey, externalUserPathIndex:
		return selector.component == child.component, nil
	case externalUserPathWildcard:
		return true, nil
	case externalUserPathUnion:
		for _, member := range selector.union {
			if member.slice != nil && member.slice.step == 0 {
				return false, fmt.Errorf("slice step is zero")
			}
		}
		for _, member := range selector.union {
			switch {
			case member.component != nil && *member.component == child.component:
				return true, nil
			case member.slice != nil:
				matched, err := externalUserSliceMatches(*member.slice, parent, child.component)
				if err != nil || matched {
					return matched, err
				}
			}
		}
		return false, nil
	case externalUserPathFilter:
		value, err := evaluateExternalUserExpression(selector.expression, child.value)
		if err == errExternalUserExpressionNoValue {
			return false, nil
		}
		return err == nil && externalUserTruthy(value), err
	case externalUserPathScript:
		return false, fmt.Errorf("script selector components are not prepared")
	default:
		return false, nil
	}
}

func externalUserSliceMatches(slice externalUserPathSlice, parent any, component string) (bool, error) {
	index, err := strconv.Atoi(component)
	if err != nil || strconv.Itoa(index) != component {
		return false, nil
	}
	if slice.step == 0 {
		return false, fmt.Errorf("slice step is zero")
	}

	length := float64(externalUserPathChildCount(parent))
	from := 0.0
	if slice.start != nil {
		from = *slice.start
		if from < 0 {
			from = length + from
		}
	}
	to := length + 1
	if slice.end != nil {
		to = *slice.end
		if to < 0 {
			to = length + to
		}
	}
	limit := to - 1
	candidate := float64(index)
	if slice.step > 0 {
		return candidate >= from && candidate <= limit && math.Mod(candidate-from, slice.step) == 0, nil
	}
	return candidate <= from && candidate >= limit && math.Mod(from-candidate, -slice.step) == 0, nil
}

func externalUserExpressionComponents(value any, visit func(string)) error {
	if visit == nil {
		return errExternalUserExpressionUnsafeCoercion
	}
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			if component, ok := externalUserLuaString(item); ok {
				visit(component)
				continue
			}
			return errExternalUserExpressionUnsafeCoercion
		}
		return nil
	case []string:
		for _, component := range current {
			visit(component)
		}
		return nil
	default:
		component, ok := externalUserLuaString(value)
		if !ok {
			return errExternalUserExpressionUnsafeCoercion
		}
		visit(component)
		return nil
	}
}

func (budget *externalUserTraversalBudget) externalUserScriptComponentSet(
	value any,
) (map[string]struct{}, error) {
	if budget == nil {
		return nil, fmt.Errorf("JSONPath script-component budget is unavailable")
	}
	componentCount := 1
	switch current := value.(type) {
	case []any:
		componentCount = len(current)
	case []string:
		componentCount = len(current)
	}
	if componentCount > maxExternalUserScriptComponentVisits-budget.scriptComponentVisits {
		return nil, fmt.Errorf("JSONPath script-component visit budget exceeded")
	}

	componentBytes := 0
	componentBytesExceeded := false
	if err := externalUserExpressionComponents(value, func(component string) {
		if componentBytesExceeded {
			return
		}
		if len(component) > maxExternalUserScriptComponentBytes-componentBytes {
			componentBytesExceeded = true
			return
		}
		componentBytes += len(component)
	}); err != nil {
		return nil, err
	}
	if componentBytesExceeded || componentBytes > maxExternalUserScriptComponentBytes-budget.scriptComponentBytes {
		return nil, fmt.Errorf("JSONPath script-component byte budget exceeded")
	}

	components := make(map[string]struct{}, componentCount)
	if err := externalUserExpressionComponents(value, func(component string) {
		components[component] = struct{}{}
	}); err != nil {
		return nil, err
	}
	budget.scriptComponentVisits += componentCount
	budget.scriptComponentBytes += componentBytes
	return components, nil
}

func findExternalUserNodes(
	node externalUserPathNode,
	selector externalUserPathSelector,
	budget *externalUserTraversalBudget,
	depth int,
) ([]externalUserPathNode, error) {
	if depth > maxExternalUserJSONPathRecursiveDepth {
		return nil, fmt.Errorf("JSONPath recursive-depth budget exceeded")
	}
	children, err := budget.externalUserPathChildren(node.value)
	if err != nil {
		return nil, err
	}
	matches, err := selectExternalUserPathChildren(node, selector, children, budget)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		path := make([]string, len(node.path)+1)
		copy(path, node.path)
		path[len(node.path)] = child.component
		descendants, err := findExternalUserNodes(
			externalUserPathNode{value: child.value, path: path},
			selector,
			budget,
			depth+1,
		)
		if err != nil {
			return nil, err
		}
		matches = append(matches, descendants...)
	}
	return matches, nil
}

func deduplicateExternalUserPathNodes(nodes []externalUserPathNode) []externalUserPathNode {
	seen := make(map[string]struct{}, len(nodes))
	deduplicated := make([]externalUserPathNode, 0, len(nodes))
	for _, node := range nodes {
		key := externalUserCanonicalPath(node.path)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduplicated = append(deduplicated, node)
	}
	return deduplicated
}

func externalUserCanonicalPath(path []string) string {
	var canonical strings.Builder
	for _, component := range path {
		canonical.WriteString(strconv.Itoa(len(component)))
		canonical.WriteByte(':')
		canonical.WriteString(component)
	}
	return canonical.String()
}

type externalUserMatches []any

func parseExternalUserJSONPath(path string) ([]externalUserPathStep, error) {
	if len(path) > maxExternalUserJSONPathBytes {
		return nil, fmt.Errorf("JSONPath byte budget exceeded")
	}
	parser := externalUserJSONPathParser{input: path}
	parser.skipSpace()
	if parser.position == len(parser.input) {
		return nil, fmt.Errorf("missing selector")
	}

	hasRoot := parser.consumeByte('$')
	if hasRoot && parser.position == len(parser.input) {
		return nil, nil
	}
	if hasRoot {
		end := parser
		end.skipSpace()
		if end.position == len(end.input) {
			return nil, nil
		}
	}

	steps := make([]externalUserPathStep, 0, strings.Count(path, ".")+1)
	first := true
	for parser.position < len(parser.input) {
		step := externalUserPathStep{}
		requireBareSelector := false
		if first && !hasRoot {
			if parser.peekByte() == '.' {
				return nil, fmt.Errorf("path cannot start with child operator")
			}
		} else {
			switch parser.peekByte() {
			case '.':
				parser.position++
				if parser.consumeByte('.') {
					step.recursive = true
				} else {
					requireBareSelector = true
				}
				if parser.position >= len(parser.input) {
					return nil, fmt.Errorf("missing selector")
				}
			case '[':
				// A bracket selector directly follows the root or the previous selector.
			default:
				if first || len(steps) > 0 {
					return nil, fmt.Errorf("missing child operator")
				}
			}
		}

		var err error
		if parser.peekByte() == '[' {
			if requireBareSelector {
				return nil, fmt.Errorf("bracket selector cannot follow child operator")
			}
			step.selector, parser.position, err = parseExternalUserBracketSelector(parser.input, parser.position)
		} else {
			step.selector, parser.position, err = parseExternalUserBareSelector(parser.input, parser.position)
		}
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
		if len(steps) > maxExternalUserJSONPathSteps {
			return nil, fmt.Errorf("JSONPath selector-step budget exceeded")
		}
		first = false
		if parser.position < len(parser.input) && isExternalUserJSONPathSpace(parser.peekByte()) {
			end := parser
			end.skipSpace()
			if end.position == len(end.input) {
				parser.position = end.position
			}
		}
	}
	return steps, nil
}

func parseExternalUserBareSelector(path string, position int) (externalUserPathSelector, int, error) {
	parser := externalUserJSONPathParser{input: path, position: position}
	if parser.consumeByte('*') {
		parser.skipSpace()
		return externalUserPathSelector{kind: externalUserPathWildcard}, parser.position, nil
	}
	if value, ok := parser.parseChildIndex(); ok {
		return externalUserPathSelector{kind: externalUserPathIndex, component: value}, parser.position, nil
	}
	if value, ok := parser.parseName(); ok {
		return externalUserPathSelector{kind: externalUserPathKey, component: value}, parser.position, nil
	}
	return externalUserPathSelector{}, position, fmt.Errorf("invalid selector")
}

func parseExternalUserBracketSelector(
	path string,
	position int,
) (externalUserPathSelector, int, error) {
	close, err := findExternalUserBracketClose(path, position)
	if err != nil {
		return externalUserPathSelector{}, position, err
	}
	selector, err := parseExternalUserBracketContent(path[position+1 : close])
	if err != nil {
		return externalUserPathSelector{}, position, err
	}
	return selector, close + 1, nil
}

type externalUserJSONPathParser struct {
	input           string
	position        int
	expressionDepth int
}

func (parser *externalUserJSONPathParser) peekByte() byte {
	if parser.position >= len(parser.input) {
		return 0
	}
	return parser.input[parser.position]
}

func (parser *externalUserJSONPathParser) consumeByte(value byte) bool {
	if parser.peekByte() != value {
		return false
	}
	parser.position++
	return true
}

func (parser *externalUserJSONPathParser) skipSpace() {
	for parser.position < len(parser.input) && isExternalUserJSONPathSpace(parser.input[parser.position]) {
		parser.position++
	}
}

func (parser *externalUserJSONPathParser) parseChildIndex() (string, bool) {
	start := parser.position
	if parser.peekByte() == '+' || parser.peekByte() == '-' {
		parser.position++
	}
	digits := parser.position
	for parser.position < len(parser.input) && parser.input[parser.position] >= '0' && parser.input[parser.position] <= '9' {
		parser.position++
	}
	if parser.position == digits {
		parser.position = start
		return "", false
	}
	value := parser.input[start:parser.position]
	parser.skipSpace()
	return value, true
}

func (parser *externalUserJSONPathParser) parseName() (string, bool) {
	if !isExternalUserPathNameStart(parser.peekByte()) {
		return "", false
	}
	start := parser.position
	parser.position++
	for isExternalUserPathNameContinue(parser.peekByte()) {
		parser.position++
	}
	value := parser.input[start:parser.position]
	parser.skipSpace()
	return value, true
}

func isExternalUserJSONPathSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n'
}

func isExternalUserPathNameStart(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value == '_'
}

func isExternalUserPathNameContinue(value byte) bool {
	return isExternalUserPathNameStart(value) || value >= '0' && value <= '9'
}

func findExternalUserBracketClose(path string, open int) (int, error) {
	depth := 1
	quote := byte(0)
	escaped := false
	for position := open + 1; position < len(path); position++ {
		character := path[position]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return position, nil
			}
		}
	}
	return 0, fmt.Errorf("unclosed bracket selector")
}

func parseExternalUserBracketContent(content string) (externalUserPathSelector, error) {
	parser := externalUserJSONPathParser{input: content}
	parser.skipSpace()
	if parser.consumeByte('*') {
		if parser.position != len(parser.input) {
			return externalUserPathSelector{}, fmt.Errorf("invalid wildcard selector")
		}
		return externalUserPathSelector{kind: externalUserPathWildcard}, nil
	}

	if parser.consumeByte('?') {
		if !parser.consumeByte('(') {
			return externalUserPathSelector{}, fmt.Errorf("invalid filter selector")
		}
		parser.skipSpace()
		expression, err := parseExternalUserExpression(&parser)
		if err != nil || !parser.consumeByte(')') {
			return externalUserPathSelector{}, fmt.Errorf("invalid filter expression")
		}
		if err := validateExternalUserExpressionComplexity(expression); err != nil {
			return externalUserPathSelector{}, err
		}
		parser.skipSpace()
		if parser.position != len(parser.input) {
			return externalUserPathSelector{}, fmt.Errorf("invalid filter selector")
		}
		return externalUserPathSelector{kind: externalUserPathFilter, expression: expression}, nil
	}

	if parser.consumeByte('(') {
		parser.skipSpace()
		expression, err := parseExternalUserExpression(&parser)
		if err != nil || !parser.consumeByte(')') {
			return externalUserPathSelector{}, fmt.Errorf("invalid script expression")
		}
		if err := validateExternalUserExpressionComplexity(expression); err != nil {
			return externalUserPathSelector{}, err
		}
		parser.skipSpace()
		if parser.position != len(parser.input) {
			return externalUserPathSelector{}, fmt.Errorf("invalid script selector")
		}
		return externalUserPathSelector{kind: externalUserPathScript, expression: expression}, nil
	}

	members := make([]externalUserPathUnionMember, 0, strings.Count(content, ",")+1)
	for {
		member, err := parseExternalUserUnionMember(&parser)
		if err != nil {
			return externalUserPathSelector{}, err
		}
		members = append(members, member)
		if len(members) > maxExternalUserJSONPathSelectorTerms {
			return externalUserPathSelector{}, fmt.Errorf("JSONPath selector-term budget exceeded")
		}
		if !parser.consumeByte(',') {
			break
		}
		parser.skipSpace()
	}
	if parser.position != len(parser.input) {
		return externalUserPathSelector{}, fmt.Errorf("invalid union selector")
	}
	return externalUserPathSelector{kind: externalUserPathUnion, union: members}, nil
}

func parseExternalUserUnionMember(parser *externalUserJSONPathParser) (externalUserPathUnionMember, error) {
	if parser.peekByte() == '\'' || parser.peekByte() == '"' {
		component, err := parseExternalUserQuotedString(parser)
		if err != nil {
			return externalUserPathUnionMember{}, err
		}
		return externalUserPathUnionMember{component: &component}, nil
	}

	startRaw, hasStart := parser.parseChildIndex()
	if parser.consumeByte(':') {
		parser.skipSpace()
		var start *float64
		if hasStart {
			value, err := strconv.ParseFloat(startRaw, 64)
			if err != nil || !isFiniteExternalUserNumber(value) {
				return externalUserPathUnionMember{}, fmt.Errorf("invalid slice start")
			}
			start = &value
		}

		endRaw, hasEnd := parser.parseChildIndex()
		var end *float64
		if hasEnd {
			value, err := strconv.ParseFloat(endRaw, 64)
			if err != nil || !isFiniteExternalUserNumber(value) {
				return externalUserPathUnionMember{}, fmt.Errorf("invalid slice end")
			}
			end = &value
		}
		step := 1.0
		if parser.consumeByte(':') {
			parser.skipSpace()
			stepRaw, ok := parser.parseChildIndex()
			if !ok {
				return externalUserPathUnionMember{}, fmt.Errorf("invalid slice step")
			}
			value, err := strconv.ParseFloat(stepRaw, 64)
			if err != nil || !isFiniteExternalUserNumber(value) {
				return externalUserPathUnionMember{}, fmt.Errorf("invalid slice step")
			}
			step = value
		}
		return externalUserPathUnionMember{slice: &externalUserPathSlice{start: start, end: end, step: step}}, nil
	}
	if !hasStart {
		return externalUserPathUnionMember{}, fmt.Errorf("invalid union member")
	}
	return externalUserPathUnionMember{component: &startRaw}, nil
}

func parseExternalUserQuotedString(parser *externalUserJSONPathParser) (string, error) {
	quote := parser.peekByte()
	if quote != '\'' && quote != '"' {
		return "", fmt.Errorf("expected quoted string")
	}
	parser.position++
	var value strings.Builder
	for parser.position < len(parser.input) {
		character := parser.input[parser.position]
		parser.position++
		if character == quote {
			parser.skipSpace()
			return value.String(), nil
		}
		if character == '\\' && parser.position < len(parser.input) {
			escaped := parser.input[parser.position]
			parser.position++
			if escaped == quote {
				value.WriteByte(quote)
			} else {
				value.WriteByte('\\')
				value.WriteByte(escaped)
			}
			continue
		}
		value.WriteByte(character)
	}
	return "", fmt.Errorf("unclosed quoted string")
}

type externalUserExpressionKind uint8

const (
	externalUserExpressionLiteral externalUserExpressionKind = iota
	externalUserExpressionVariable
	externalUserExpressionLength
	externalUserExpressionBinary
)

type externalUserExpression struct {
	kind     externalUserExpressionKind
	literal  any
	members  []string
	operator string
	left     *externalUserExpression
	right    *externalUserExpression
}

func validateExternalUserExpressionComplexity(expression *externalUserExpression) error {
	type expressionNode struct {
		expression *externalUserExpression
		depth      int
	}
	stack := []expressionNode{{expression: expression, depth: 1}}
	nodes := 0
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if current.expression == nil {
			continue
		}
		nodes++
		if current.expression.kind == externalUserExpressionVariable {
			nodes += len(current.expression.members)
		}
		if nodes > maxExternalUserJSONPathExpressionNodes {
			return fmt.Errorf("JSONPath expression-node budget exceeded")
		}
		if current.depth > maxExternalUserJSONPathExpressionDepth {
			return fmt.Errorf("JSONPath expression-depth budget exceeded")
		}
		if current.expression.kind == externalUserExpressionBinary {
			stack = append(stack,
				expressionNode{expression: current.expression.left, depth: current.depth + 1},
				expressionNode{expression: current.expression.right, depth: current.depth + 1},
			)
		}
	}
	return nil
}

var (
	errExternalUserExpressionNoValue        = fmt.Errorf("expression value is not set")
	errExternalUserExpressionUnsafeCoercion = fmt.Errorf("expression value cannot be safely coerced")
)

func parseExternalUserExpression(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	return parseExternalUserLogicalOr(parser)
}

func parseExternalUserLogicalOr(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	return parseExternalUserBinary(parser, parseExternalUserLogicalAnd, []string{"OR", "||"}, true)
}

func parseExternalUserLogicalAnd(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	return parseExternalUserBinary(parser, parseExternalUserEquality, []string{"AND", "&&"}, true)
}

func parseExternalUserEquality(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	return parseExternalUserBinary(parser, parseExternalUserRelational, []string{"==", "!=", "<>", "="}, false)
}

func parseExternalUserRelational(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	left, err := parseExternalUserAdditive(parser)
	if err != nil {
		return nil, err
	}
	for {
		operator := ""
		switch {
		case parser.consumeString("<="):
			operator = "<="
		case parser.consumeString(">="):
			operator = ">="
		case parser.peekByte() == '<' && !strings.HasPrefix(parser.input[parser.position:], "<>"):
			parser.position++
			operator = "<"
		case parser.consumeByte('>'):
			operator = ">"
		default:
			return left, nil
		}
		parser.skipSpace()
		right, err := parseExternalUserAdditive(parser)
		if err != nil {
			return nil, err
		}
		left = &externalUserExpression{
			kind: externalUserExpressionBinary, operator: operator, left: left, right: right,
		}
	}
}

func parseExternalUserAdditive(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	return parseExternalUserBinary(parser, parseExternalUserMultiplicative, []string{"+", "-"}, false)
}

func parseExternalUserMultiplicative(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	return parseExternalUserBinary(parser, parseExternalUserFactor, []string{"*", "/", "%"}, false)
}

type externalUserExpressionParseFunc func(*externalUserJSONPathParser) (*externalUserExpression, error)

func parseExternalUserBinary(
	parser *externalUserJSONPathParser,
	parseOperand externalUserExpressionParseFunc,
	operators []string,
	caseInsensitive bool,
) (*externalUserExpression, error) {
	left, err := parseOperand(parser)
	if err != nil {
		return nil, err
	}
	for {
		operator := parser.consumeOperator(operators, caseInsensitive)
		if operator == "" {
			return left, nil
		}
		parser.skipSpace()
		right, err := parseOperand(parser)
		if err != nil {
			return nil, err
		}
		left = &externalUserExpression{
			kind: externalUserExpressionBinary, operator: operator, left: left, right: right,
		}
	}
}

func (parser *externalUserJSONPathParser) consumeString(value string) bool {
	if !strings.HasPrefix(parser.input[parser.position:], value) {
		return false
	}
	parser.position += len(value)
	return true
}

func (parser *externalUserJSONPathParser) consumeOperator(operators []string, caseInsensitive bool) string {
	for _, operator := range operators {
		end := parser.position + len(operator)
		if end > len(parser.input) {
			continue
		}
		candidate := parser.input[parser.position:end]
		if candidate == operator || caseInsensitive && strings.EqualFold(candidate, operator) {
			parser.position = end
			return candidate
		}
	}
	return ""
}

func parseExternalUserFactor(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	if parser.peekByte() == '\'' || parser.peekByte() == '"' {
		value, err := parseExternalUserQuotedString(parser)
		if err != nil {
			return nil, err
		}
		return &externalUserExpression{kind: externalUserExpressionLiteral, literal: value}, nil
	}
	if parser.consumeFold("true") {
		parser.skipSpace()
		return &externalUserExpression{kind: externalUserExpressionLiteral, literal: true}, nil
	}
	if parser.consumeFold("false") {
		parser.skipSpace()
		return &externalUserExpression{kind: externalUserExpressionLiteral, literal: false}, nil
	}
	if parser.peekByte() == '@' {
		return parseExternalUserVariable(parser)
	}
	if parser.consumeByte('(') {
		parser.expressionDepth++
		if parser.expressionDepth > maxExternalUserJSONPathExpressionDepth {
			return nil, fmt.Errorf("JSONPath expression nesting budget exceeded")
		}
		parser.skipSpace()
		expression, err := parseExternalUserExpression(parser)
		parser.expressionDepth--
		if err != nil || !parser.consumeByte(')') {
			return nil, fmt.Errorf("invalid parenthesized expression")
		}
		parser.skipSpace()
		return expression, nil
	}
	if value, ok := parser.parseNumber(); ok {
		if !isFiniteExternalUserNumber(value) {
			return nil, fmt.Errorf("non-finite numeric literal")
		}
		return &externalUserExpression{kind: externalUserExpressionLiteral, literal: value}, nil
	}
	return nil, fmt.Errorf("invalid expression factor")
}

func (parser *externalUserJSONPathParser) consumeFold(value string) bool {
	end := parser.position + len(value)
	if end > len(parser.input) || !strings.EqualFold(parser.input[parser.position:end], value) {
		return false
	}
	parser.position = end
	return true
}

func (parser *externalUserJSONPathParser) parseNumber() (float64, bool) {
	start := parser.position
	if strings.HasPrefix(parser.input[start:], "0x") || strings.HasPrefix(parser.input[start:], "0X") {
		parser.position += 2
		digits := parser.position
		for isExternalUserHexDigit(parser.peekByte()) {
			parser.position++
		}
		if parser.position == digits {
			parser.position = start
			return 0, false
		}
		value := externalUserHexFloat(parser.input[digits:parser.position])
		parser.skipSpace()
		return value, true
	}

	if parser.peekByte() == '+' || parser.peekByte() == '-' {
		parser.position++
	}
	digitsBefore := parser.position
	for parser.peekByte() >= '0' && parser.peekByte() <= '9' {
		parser.position++
	}
	hasDigitsBefore := parser.position > digitsBefore
	hasDot := parser.consumeByte('.')
	digitsAfter := parser.position
	if hasDot {
		for parser.peekByte() >= '0' && parser.peekByte() <= '9' {
			parser.position++
		}
	}
	if !hasDigitsBefore && (!hasDot || parser.position == digitsAfter) {
		parser.position = start
		return 0, false
	}
	if parser.peekByte() == 'e' || parser.peekByte() == 'E' {
		exponent := parser.position
		parser.position++
		if parser.peekByte() == '+' || parser.peekByte() == '-' {
			parser.position++
		}
		digits := parser.position
		for parser.peekByte() >= '0' && parser.peekByte() <= '9' {
			parser.position++
		}
		if parser.position == digits {
			parser.position = exponent
		}
	}
	value, _ := strconv.ParseFloat(parser.input[start:parser.position], 64)
	parser.skipSpace()
	return value, true
}

func isExternalUserHexDigit(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func externalUserHexFloat(value string) float64 {
	number := 0.0
	for index := range len(value) {
		digit := value[index]
		number *= 16
		switch {
		case digit >= '0' && digit <= '9':
			number += float64(digit - '0')
		case digit >= 'a' && digit <= 'f':
			number += float64(digit-'a') + 10
		default:
			number += float64(digit-'A') + 10
		}
	}
	return number
}

func parseExternalUserVariable(parser *externalUserJSONPathParser) (*externalUserExpression, error) {
	parser.position++
	if parser.consumeByte('.') {
		if parser.consumeString("length") {
			parser.skipSpace()
			return &externalUserExpression{kind: externalUserExpressionLength}, nil
		}
		member, ok := parser.parseName()
		if !ok {
			return nil, fmt.Errorf("invalid variable")
		}
		members := []string{member}
		for parser.consumeByte('.') {
			member, ok = parser.parseName()
			if !ok {
				return nil, fmt.Errorf("invalid variable")
			}
			members = append(members, member)
		}
		return &externalUserExpression{kind: externalUserExpressionVariable, members: members}, nil
	}
	if parser.consumeByte('[') {
		parser.skipSpace()
		member, err := parseExternalUserQuotedString(parser)
		if err != nil || !parser.consumeByte(']') {
			return nil, fmt.Errorf("invalid bracket variable")
		}
		parser.skipSpace()
		return &externalUserExpression{kind: externalUserExpressionVariable, members: []string{member}}, nil
	}
	return nil, fmt.Errorf("invalid variable")
}

func evaluateExternalUserExpression(expression *externalUserExpression, object any) (any, error) {
	if expression == nil {
		return nil, fmt.Errorf("expression is not set")
	}
	switch expression.kind {
	case externalUserExpressionLiteral:
		return expression.literal, nil
	case externalUserExpressionLength:
		switch current := object.(type) {
		case map[string]any:
			return float64(len(current)), nil
		case []any:
			return float64(len(current)), nil
		case []string:
			return float64(len(current)), nil
		default:
			return nil, fmt.Errorf("length object is not a table")
		}
	case externalUserExpressionVariable:
		current := object
		// lua-jsonpath 1.0-1 advances through its captured variable AST by two.
		// Preserve that observable behavior for nested member expressions.
		for index := 0; index < len(expression.members); index += 2 {
			values, ok := current.(map[string]any)
			if !ok {
				return nil, errExternalUserExpressionNoValue
			}
			current, ok = values[expression.members[index]]
			if !ok || current == nil {
				return nil, errExternalUserExpressionNoValue
			}
		}
		return current, nil
	case externalUserExpressionBinary:
		return evaluateExternalUserBinaryExpression(expression, object)
	default:
		return nil, fmt.Errorf("unknown expression")
	}
}

func evaluateExternalUserBinaryExpression(expression *externalUserExpression, object any) (any, error) {
	left, err := evaluateExternalUserExpression(expression.left, object)
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, errExternalUserExpressionNoValue
	}
	right, err := evaluateExternalUserExpression(expression.right, object)
	if err != nil {
		return nil, err
	}
	if right == nil {
		return nil, errExternalUserExpressionNoValue
	}
	if externalUserNativeNumberIsNonFinite(left) || externalUserNativeNumberIsNonFinite(right) {
		return nil, fmt.Errorf("non-finite expression operand")
	}

	switch strings.ToUpper(expression.operator) {
	case "+", "-", "*", "/", "%":
		leftNumber, leftOK := externalUserNumber(left)
		rightNumber, rightOK := externalUserNumber(right)
		if !leftOK || !rightOK {
			return nil, fmt.Errorf("arithmetic operand is not a number")
		}
		var result float64
		switch expression.operator {
		case "+":
			result = leftNumber + rightNumber
		case "-":
			result = leftNumber - rightNumber
		case "*":
			result = leftNumber * rightNumber
		case "/":
			result = leftNumber / rightNumber
		default:
			result = math.Mod(leftNumber, rightNumber)
		}
		if !isFiniteExternalUserNumber(result) {
			return nil, fmt.Errorf("non-finite arithmetic result")
		}
		return result, nil
	case "AND", "&&":
		return externalUserTruthy(left) && externalUserTruthy(right), nil
	case "OR", "||":
		return externalUserTruthy(left) || externalUserTruthy(right), nil
	}

	right, err = coerceExternalUserExpressionOperand(left, right)
	if err != nil {
		if err == errExternalUserExpressionUnsafeCoercion {
			return nil, err
		}
		if expression.operator == "=" || expression.operator == "==" {
			return false, nil
		}
		if expression.operator == "!=" || expression.operator == "<>" {
			return true, nil
		}
		return nil, err
	}
	switch expression.operator {
	case "=", "==":
		return externalUserEqual(left, right), nil
	case "!=", "<>":
		return !externalUserEqual(left, right), nil
	case "<", "<=", ">", ">=":
		comparison, err := compareExternalUserValues(left, right)
		if err != nil {
			return nil, err
		}
		switch expression.operator {
		case "<":
			return comparison < 0, nil
		case "<=":
			return comparison <= 0, nil
		case ">":
			return comparison > 0, nil
		default:
			return comparison >= 0, nil
		}
	default:
		return nil, fmt.Errorf("unknown expression operator %q", expression.operator)
	}
}

func externalUserTruthy(value any) bool {
	if value == nil {
		return false
	}
	if boolean, ok := value.(bool); ok {
		return boolean
	}
	return true
}

func externalUserNumber(value any) (float64, bool) {
	number, ok := externalUserNumericValue(value)
	return number, ok && isFiniteExternalUserNumber(number)
}

func externalUserNumericValue(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int8:
		return float64(number), true
	case int16:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint8:
		return float64(number), true
	case uint16:
		return float64(number), true
	case uint32:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float32:
		return float64(number), true
	case float64:
		return number, true
	case string:
		number = strings.Trim(number, " \t\n\r\v\f")
		if strings.HasPrefix(number, "0x") || strings.HasPrefix(number, "0X") {
			if len(number) == 2 {
				return 0, false
			}
			for index := 2; index < len(number); index++ {
				if !isExternalUserHexDigit(number[index]) {
					return 0, false
				}
			}
			return externalUserHexFloat(number[2:]), true
		}
		parsed, err := strconv.ParseFloat(number, 64)
		return parsed, err == nil || math.IsInf(parsed, 0)
	default:
		return 0, false
	}
}

func isFiniteExternalUserNumber(number float64) bool {
	return !math.IsNaN(number) && !math.IsInf(number, 0)
}

func externalUserNativeNumberIsNonFinite(value any) bool {
	switch number := value.(type) {
	case float32:
		return !isFiniteExternalUserNumber(float64(number))
	case float64:
		return !isFiniteExternalUserNumber(number)
	default:
		return false
	}
}

func coerceExternalUserExpressionOperand(left, right any) (any, error) {
	switch left.(type) {
	case bool:
		return externalUserTruthy(right), nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		value, ok := externalUserNumber(right)
		if !ok {
			if number, numeric := externalUserNumericValue(right); numeric && !isFiniteExternalUserNumber(number) {
				return nil, errExternalUserExpressionUnsafeCoercion
			}
			return nil, fmt.Errorf("comparison operand is not a number")
		}
		return value, nil
	default:
		if !externalUserTruthy(right) {
			return "", nil
		}
		value, ok := externalUserLuaString(right)
		if !ok {
			return nil, errExternalUserExpressionUnsafeCoercion
		}
		return value, nil
	}
}

func externalUserLuaString(value any) (string, bool) {
	switch current := value.(type) {
	case string:
		return current, true
	case bool:
		return strconv.FormatBool(current), true
	default:
		number, ok := externalUserNumber(current)
		if !ok {
			return "", false
		}
		return strconv.FormatFloat(number, 'g', -1, 64), true
	}
}

func externalUserEqual(left, right any) bool {
	switch current := left.(type) {
	case string:
		value, ok := right.(string)
		return ok && current == value
	case bool:
		value, ok := right.(bool)
		return ok && current == value
	default:
		leftNumber, leftOK := externalUserNumber(left)
		rightNumber, rightOK := externalUserNumber(right)
		return leftOK && rightOK && leftNumber == rightNumber
	}
}

func compareExternalUserValues(left, right any) (int, error) {
	if leftString, ok := left.(string); ok {
		rightString, ok := right.(string)
		if !ok {
			return 0, fmt.Errorf("string comparison type mismatch")
		}
		return strings.Compare(leftString, rightString), nil
	}
	leftNumber, leftOK := externalUserNumber(left)
	rightNumber, rightOK := externalUserNumber(right)
	if !leftOK || !rightOK {
		return 0, fmt.Errorf("comparison operands are not ordered values")
	}
	switch {
	case leftNumber < rightNumber:
		return -1, nil
	case leftNumber > rightNumber:
		return 1, nil
	default:
		return 0, nil
	}
}

func validateExternalUserLabelField(path string) error {
	if path == "" {
		return nil
	}
	if _, err := parseExternalUserJSONPath(path); err != nil {
		return fmt.Errorf("invalid external_user_label_field %q: %w", path, err)
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

func extractValues(
	value any, budget *externalUserLabelBudget, bytesReserved bool,
) ([]string, error) {
	switch v := value.(type) {
	case []string:
		if len(v) > budget.remainingValues() {
			return nil, fmt.Errorf("label value budget exceeded")
		}
		values := make([]string, 0, len(v))
		for _, item := range v {
			var err error
			values, err = budget.appendValue(values, item, bytesReserved)
			if err != nil {
				return nil, err
			}
		}
		return values, nil
	case []any:
		if len(v) > budget.remainingValues() {
			return nil, fmt.Errorf("label value budget exceeded")
		}
		values := make([]string, 0, len(v))
		for _, item := range v {
			if item == nil {
				if err := budget.reserveValue(); err != nil {
					return nil, err
				}
				continue
			}
			label, ok := item.(string)
			if !ok && !isExternalUserNonStringScalar(item) {
				return nil, fmt.Errorf("label reference value cannot be safely coerced")
			}
			if !ok {
				if err := budget.reserveValue(); err != nil {
					return nil, err
				}
				continue
			}
			var err error
			values, err = budget.appendValue(values, label, bytesReserved)
			if err != nil {
				return nil, err
			}
		}
		return values, nil
	case string:
		if !bytesReserved {
			if err := budget.reserveBytes(len(v)); err != nil {
				return nil, err
			}
		}
		return extractStringValues(v, budget)
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return nil, budget.reserveValue()
	default:
		return nil, fmt.Errorf("label reference value cannot be safely coerced")
	}
}

func isExternalUserNonStringScalar(value any) bool {
	switch value.(type) {
	case bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	default:
		return false
	}
}

func extractStringValues(value string, budget *externalUserLabelBudget) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	if strings.HasPrefix(value, "[") {
		var values []any
		if err := json.Unmarshal([]byte(value), &values); err != nil {
			logger.Warnf("failed to decode labels [%s] as array, err: %v", value, err)
			return nil, nil
		}
		return extractValues(values, budget, true)
	}

	if strings.Contains(value, ",") {
		if strings.Count(value, ",") >= budget.remainingValues() {
			return nil, fmt.Errorf("label value budget exceeded")
		}
		parts := strings.Split(value, ",")
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				var err error
				values, err = budget.appendValue(values, part, true)
				if err != nil {
					return nil, err
				}
			}
		}
		return values, nil
	}

	logger.Infof("the string value can not parsed by json or segmented_text")
	return budget.appendValue(nil, value, true)
}
