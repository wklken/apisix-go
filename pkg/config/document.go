package config

import (
	"bytes"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/json"
	"go.yaml.in/yaml/v3"
)

type forbiddenYAMLKind uint8

const (
	forbiddenMerge forbiddenYAMLKind = iota
	forbiddenAlias
	forbiddenAnchor
)

type expandedTemplate struct {
	value    string
	names    []string
	expanded bool
}

func parseDocument(data []byte, source FieldSource, env map[string]string) (*valueNode, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if err == io.EOF {
			return &valueNode{
				kind: nodeMapping, mapping: map[string]*valueNode{},
				source: source, pathBase: documentPathBase(source),
			}, nil
		}
		return nil, fmt.Errorf("decode YAML document: invalid YAML")
	}

	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("multiple YAML documents are not supported")
	} else if err != io.EOF {
		return nil, fmt.Errorf("decode trailing YAML document: invalid YAML")
	}

	if len(document.Content) != 1 {
		return nil, fmt.Errorf("decode YAML document: invalid document root")
	}
	root := document.Content[0]
	if err := rejectForbiddenYAML(root); err != nil {
		return nil, err
	}
	return convertYAMLNode(root, "", source, documentPathBase(source), env, nil)
}

func readConfigDocument(path string, source FieldSource, env map[string]string) (*valueNode, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read static configuration %s: %w", path, err)
	}
	document, err := parseDocument(data, source, env)
	if err != nil {
		return nil, fmt.Errorf("parse static configuration %s: %w", path, err)
	}
	return document, nil
}

func documentPathBase(source FieldSource) string {
	if source.Kind == SourceDefaultFile || source.Kind == SourceOverrideFile {
		return filepath.Dir(source.Origin)
	}
	return ""
}

func rejectForbiddenYAML(root *yaml.Node) error {
	for _, kind := range []forbiddenYAMLKind{forbiddenMerge, forbiddenAlias, forbiddenAnchor} {
		if path, found := findForbiddenYAML(root, "", kind); found {
			location := displayFieldPath(path)
			switch kind {
			case forbiddenMerge:
				return fmt.Errorf("YAML merge key at %s is not supported", location)
			case forbiddenAlias:
				return fmt.Errorf("YAML alias at %s is not supported", location)
			case forbiddenAnchor:
				return fmt.Errorf("YAML anchor at %s is not supported", location)
			}
		}
	}
	return nil
}

func findForbiddenYAML(node *yaml.Node, path string, kind forbiddenYAMLKind) (string, bool) {
	if node == nil {
		return "", false
	}
	if kind == forbiddenAlias && node.Kind == yaml.AliasNode {
		return path, true
	}
	if kind == forbiddenAnchor && node.Anchor != "" {
		return path, true
	}

	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if foundPath, found := findForbiddenYAML(child, path, kind); found {
				return foundPath, true
			}
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			value := node.Content[index+1]
			if kind == forbiddenMerge && key.ShortTag() == "!!merge" {
				return path, true
			}
			keyPath := yamlKeyPath(path, key)
			if foundPath, found := findForbiddenYAML(key, keyPath, kind); found {
				return foundPath, true
			}
			if foundPath, found := findForbiddenYAML(value, keyPath, kind); found {
				return foundPath, true
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if foundPath, found := findForbiddenYAML(child, sequenceFieldPath(path, index), kind); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

func yamlKeyPath(parent string, key *yaml.Node) string {
	if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" || strings.Contains(key.Value, "${{") {
		return parent
	}
	return joinFieldPath(parent, key.Value)
}

func convertYAMLNode(
	node *yaml.Node,
	path string,
	source FieldSource,
	pathBase string,
	env map[string]string,
	inheritedNames []string,
) (*valueNode, error) {
	switch node.Kind {
	case yaml.MappingNode:
		return convertYAMLMapping(node, path, source, pathBase, env, inheritedNames)
	case yaml.SequenceNode:
		converted := &valueNode{
			kind: nodeSequence, source: sourceForTemplateNames(source, inheritedNames), pathBase: pathBase,
			sequence: make([]*valueNode, len(node.Content)),
		}
		for index, child := range node.Content {
			childNode, err := convertYAMLNode(
				child, sequenceFieldPath(path, index), source, pathBase, env, inheritedNames,
			)
			if err != nil {
				return nil, err
			}
			converted.sequence[index] = childNode
		}
		return converted, nil
	case yaml.ScalarNode:
		return convertYAMLScalar(node, path, source, pathBase, env, inheritedNames)
	default:
		return nil, fmt.Errorf("unsupported YAML node at %s", displayFieldPath(path))
	}
}

func convertYAMLMapping(
	node *yaml.Node,
	path string,
	source FieldSource,
	pathBase string,
	env map[string]string,
	inheritedNames []string,
) (*valueNode, error) {
	if len(node.Content)%2 != 0 {
		return nil, fmt.Errorf("invalid YAML mapping at %s", displayFieldPath(path))
	}
	literalKeys := make(map[string]struct{}, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" {
			return nil, fmt.Errorf("mapping key at %s must be a scalar string", displayFieldPath(path))
		}
		if _, exists := literalKeys[key.Value]; exists {
			if strings.Contains(key.Value, "${{") {
				return nil, fmt.Errorf("duplicate key at %s", displayFieldPath(path))
			}
			return nil, fmt.Errorf("duplicate key %s", displayFieldPath(joinFieldPath(path, key.Value)))
		}
		literalKeys[key.Value] = struct{}{}
	}

	converted := &valueNode{
		kind: nodeMapping, source: sourceForTemplateNames(source, inheritedNames), pathBase: pathBase,
		mapping: make(map[string]*valueNode, len(node.Content)/2),
	}
	keySources := make(map[string][]string, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		value := node.Content[index+1]
		expandedKey, err := expandAPISIXTemplates(key.Value, displayFieldPath(path), env)
		if err != nil {
			return nil, err
		}
		if previousNames, exists := keySources[expandedKey.value]; exists {
			names := unionTemplateNames(previousNames, expandedKey.names)
			return nil, fmt.Errorf(
				"expanded key collision at %s using APISIX environment %s",
				displayFieldPath(path), strings.Join(names, ","),
			)
		}
		keySources[expandedKey.value] = expandedKey.names

		childNames := unionTemplateNames(inheritedNames, expandedKey.names)
		childPath := joinFieldPath(path, expandedKey.value)
		if expandedKey.expanded {
			childPath = joinFieldPath(path, "<key>")
		}
		child, err := convertYAMLNode(value, childPath, source, pathBase, env, childNames)
		if err != nil {
			return nil, err
		}
		converted.mapping[expandedKey.value] = child
	}
	return converted, nil
}

func convertYAMLScalar(
	node *yaml.Node,
	path string,
	source FieldSource,
	pathBase string,
	env map[string]string,
	inheritedNames []string,
) (*valueNode, error) {
	converted := &valueNode{
		kind: nodeScalar, source: sourceForTemplateNames(source, inheritedNames), pathBase: pathBase,
	}
	switch node.ShortTag() {
	case "!!null":
		converted.kind = nodeNull
		return converted, nil
	case "!!bool":
		value, err := strconv.ParseBool(node.Value)
		if err != nil {
			return nil, fmt.Errorf("invalid boolean at %s", displayFieldPath(path))
		}
		converted.scalar = value
		return converted, nil
	case "!!int":
		value, ok := normalizeYAMLInteger(node.Value)
		if !ok {
			return nil, fmt.Errorf("invalid integer at %s", displayFieldPath(path))
		}
		converted.scalar = value
		return converted, nil
	case "!!float":
		if integer, ok := normalizeYAMLInteger(node.Value); ok {
			converted.scalar = integer
			return converted, nil
		}
		value, ok, nonFinite := normalizeYAMLFloat(node.Value)
		if nonFinite {
			return nil, fmt.Errorf("non-finite number at %s is not supported", displayFieldPath(path))
		}
		if !ok {
			return nil, fmt.Errorf("invalid number at %s", displayFieldPath(path))
		}
		converted.scalar = value
		return converted, nil
	}

	expanded, err := expandAPISIXTemplates(node.Value, displayFieldPath(path), env)
	if err != nil {
		return nil, err
	}
	if !expanded.expanded {
		converted.scalar = node.Value
		return converted, nil
	}
	allNames := unionTemplateNames(inheritedNames, expanded.names)
	converted.source = sourceForTemplateNames(source, allNames)
	converted.scalar = retypeExpandedScalar(expanded.value)
	return converted, nil
}

func expandAPISIXTemplates(value, path string, env map[string]string) (expandedTemplate, error) {
	var result strings.Builder
	result.Grow(len(value))
	names := make([]string, 0, 1)
	expanded := false
	cursor := 0
	for cursor < len(value) {
		startOffset := strings.Index(value[cursor:], "${{")
		if startOffset < 0 {
			result.WriteString(value[cursor:])
			break
		}
		start := cursor + startOffset
		endOffset := strings.Index(value[start+3:], "}}")
		if endOffset < 0 {
			result.WriteString(value[cursor:])
			break
		}
		end := start + 3 + endOffset
		tokenEnd := end + 2
		name, fallback, hasFallback, valid := parseTemplateExpression(value[start+3 : end])
		if !valid {
			result.WriteString(value[cursor:tokenEnd])
			cursor = tokenEnd
			continue
		}

		result.WriteString(value[cursor:start])
		replacement, present := env[name]
		if !present {
			if !hasFallback {
				return expandedTemplate{}, fmt.Errorf("expand APISIX environment at %s: %s is not set", path, name)
			}
			replacement = fallback
		}
		result.WriteString(replacement)
		names = append(names, name)
		expanded = true
		cursor = tokenEnd
	}
	expandedValue := result.String()
	if !utf8.ValidString(expandedValue) {
		return expandedTemplate{}, fmt.Errorf("expand APISIX environment at %s: result is not valid UTF-8", path)
	}
	return expandedTemplate{value: expandedValue, names: sortedTemplateNames(names), expanded: expanded}, nil
}

func parseTemplateExpression(expression string) (name, fallback string, hasFallback, valid bool) {
	expression = strings.TrimSpace(expression)
	if before, after, found := strings.Cut(expression, ":="); found {
		name = strings.TrimSpace(before)
		fallback = strings.TrimSpace(after)
		hasFallback = true
	} else {
		name = expression
	}
	if !validEnvironmentName(name) {
		return "", "", false, false
	}
	return name, fallback, hasFallback, true
}

func validEnvironmentName(name string) bool {
	if name == "" || !isEnvironmentNameStart(name[0]) {
		return false
	}
	for index := 1; index < len(name); index++ {
		if !isEnvironmentNameStart(name[index]) && (name[index] < '0' || name[index] > '9') {
			return false
		}
	}
	return true
}

func isEnvironmentNameStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func retypeExpandedScalar(value string) any {
	if value == "true" {
		return true
	}
	if value == "false" {
		return false
	}
	if number, ok := normalizeAPISIXNumber(value); ok {
		return number
	}
	return value
}

func normalizeAPISIXNumber(value string) (json.Number, bool) {
	cleaned := strings.Trim(value, " \t\n\r\v\f")
	negative := false
	if cleaned != "" && (cleaned[0] == '+' || cleaned[0] == '-') {
		negative = cleaned[0] == '-'
		cleaned = cleaned[1:]
	}
	if cleaned == "" {
		return "", false
	}

	if strings.HasPrefix(cleaned, "0b") || strings.HasPrefix(cleaned, "0B") {
		digits := cleaned[2:]
		if digits == "" || !allBinaryDigits(digits) {
			return "", false
		}
		integer, ok := new(big.Int).SetString(digits, 2)
		if !ok {
			return "", false
		}
		if negative {
			integer.Neg(integer)
		}
		return json.Number(integer.String()), true
	}

	if strings.HasPrefix(cleaned, "0x") || strings.HasPrefix(cleaned, "0X") {
		digits := cleaned[2:]
		if digits == "" || !allHexDigits(digits) {
			return "", false
		}
		integer, ok := new(big.Int).SetString(digits, 16)
		if !ok {
			return "", false
		}
		if negative {
			integer.Neg(integer)
		}
		return json.Number(integer.String()), true
	}

	mantissa := cleaned
	exponent := ""
	if exponentIndex := strings.IndexAny(cleaned, "eE"); exponentIndex >= 0 {
		if strings.ContainsAny(cleaned[exponentIndex+1:], "eE") {
			return "", false
		}
		mantissa = cleaned[:exponentIndex]
		exponent = cleaned[exponentIndex+1:]
		if !validExponent(exponent) {
			return "", false
		}
	}

	normalizedMantissa, ok := normalizeDecimalMantissa(mantissa, exponent != "")
	if !ok && exponent == "" && allDecimalDigits(mantissa) {
		normalizedMantissa, ok = trimLeadingZeros(mantissa), true
	}
	if !ok {
		return "", false
	}
	if negative {
		normalizedMantissa = "-" + normalizedMantissa
	}
	if exponent != "" {
		normalizedMantissa += "e" + exponent
	}
	return json.Number(normalizedMantissa), true
}

func normalizeYAMLInteger(value string) (json.Number, bool) {
	cleaned := strings.ReplaceAll(value, "_", "")
	negative := false
	if cleaned != "" && (cleaned[0] == '+' || cleaned[0] == '-') {
		negative = cleaned[0] == '-'
		cleaned = cleaned[1:]
	}
	if cleaned == "" {
		return "", false
	}

	base := 10
	digits := cleaned
	lower := strings.ToLower(cleaned)
	switch {
	case strings.HasPrefix(lower, "0x"):
		base, digits = 16, cleaned[2:]
	case strings.HasPrefix(lower, "0o"):
		base, digits = 8, cleaned[2:]
	case strings.HasPrefix(lower, "0b"):
		base, digits = 2, cleaned[2:]
	case len(cleaned) > 1 && cleaned[0] == '0':
		base = 8
	}
	if digits == "" {
		return "", false
	}
	integer, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return "", false
	}
	if negative {
		integer.Neg(integer)
	}
	return json.Number(integer.String()), true
}

func normalizeYAMLFloat(value string) (json.Number, bool, bool) {
	cleaned := strings.ReplaceAll(value, "_", "")
	switch strings.ToLower(cleaned) {
	case ".inf", "+.inf", "-.inf", ".nan", "+.nan", "-.nan":
		return "", false, true
	}
	negative := false
	if cleaned != "" && (cleaned[0] == '+' || cleaned[0] == '-') {
		negative = cleaned[0] == '-'
		cleaned = cleaned[1:]
	}
	if cleaned == "" {
		return "", false, false
	}

	mantissa := cleaned
	exponent := ""
	if exponentIndex := strings.IndexAny(cleaned, "eE"); exponentIndex >= 0 {
		if strings.ContainsAny(cleaned[exponentIndex+1:], "eE") {
			return "", false, false
		}
		mantissa = cleaned[:exponentIndex]
		exponent = cleaned[exponentIndex+1:]
		if !validExponent(exponent) {
			return "", false, false
		}
	}

	normalizedMantissa, ok := normalizeDecimalMantissa(mantissa, exponent != "")
	if !ok {
		return "", false, false
	}
	if negative {
		normalizedMantissa = "-" + normalizedMantissa
	}
	if exponent != "" {
		normalizedMantissa += "e" + exponent
	}
	return json.Number(normalizedMantissa), true, false
}

func normalizeDecimalMantissa(mantissa string, hasExponent bool) (string, bool) {
	if !strings.Contains(mantissa, ".") {
		if !hasExponent || !allDecimalDigits(mantissa) {
			return "", false
		}
		return trimLeadingZeros(mantissa), true
	}
	if strings.Count(mantissa, ".") != 1 {
		return "", false
	}
	integerPart, fractionPart, _ := strings.Cut(mantissa, ".")
	if integerPart == "" && fractionPart == "" {
		return "", false
	}
	if integerPart != "" && !allDecimalDigits(integerPart) {
		return "", false
	}
	if fractionPart != "" && !allDecimalDigits(fractionPart) {
		return "", false
	}
	if integerPart == "" {
		integerPart = "0"
	} else {
		integerPart = trimLeadingZeros(integerPart)
	}
	if fractionPart == "" {
		fractionPart = "0"
	}
	return integerPart + "." + fractionPart, true
}

func validExponent(exponent string) bool {
	if exponent == "" {
		return false
	}
	if exponent[0] == '+' || exponent[0] == '-' {
		exponent = exponent[1:]
	}
	return exponent != "" && allDecimalDigits(exponent)
}

func allDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func allHexDigits(value string) bool {
	for index := range len(value) {
		if value[index] >= '0' && value[index] <= '9' ||
			value[index] >= 'a' && value[index] <= 'f' ||
			value[index] >= 'A' && value[index] <= 'F' {
			continue
		}
		return false
	}
	return value != ""
}

func allBinaryDigits(value string) bool {
	for index := range len(value) {
		if value[index] != '0' && value[index] != '1' {
			return false
		}
	}
	return value != ""
}

func trimLeadingZeros(value string) string {
	trimmed := strings.TrimLeft(value, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

func sourceForTemplateNames(source FieldSource, names []string) FieldSource {
	names = sortedTemplateNames(names)
	if len(names) == 0 {
		return source
	}
	return FieldSource{Kind: SourceAPISIXEnv, Origin: strings.Join(names, ","), Explicit: true}
}

func unionTemplateNames(groups ...[]string) []string {
	var names []string
	for _, group := range groups {
		names = append(names, group...)
	}
	return sortedTemplateNames(names)
}

func sortedTemplateNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	result := sorted[:0]
	for _, name := range sorted {
		if len(result) == 0 || result[len(result)-1] != name {
			result = append(result, name)
		}
	}
	return result
}

func joinFieldPath(parent, child string) string {
	if parent == "" {
		return child
	}
	if child == "" {
		return parent
	}
	return parent + "." + child
}

func sequenceFieldPath(parent string, index int) string {
	return fmt.Sprintf("%s[%d]", parent, index)
}

func displayFieldPath(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}
