package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
)

var (
	jsonNumberPattern     = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	safeProvenanceSegment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
)

func mergeNodes(lower, upper *valueNode) *valueNode {
	if lower == nil {
		return cloneNode(upper)
	}
	if upper == nil {
		return cloneNode(lower)
	}
	if lower.kind != nodeMapping || upper.kind != nodeMapping {
		return cloneNode(upper)
	}

	merged := cloneNode(lower)
	merged.source = upper.source
	merged.pathBase = upper.pathBase
	for key, incoming := range upper.mapping {
		merged.mapping[key] = mergeNodes(merged.mapping[key], incoming)
	}
	return merged
}

func flattenProvenance(root *valueNode) Provenance {
	provenance := make(Provenance)
	if root == nil {
		return provenance
	}
	flattenNodeProvenance(root, "", true, provenance)
	return provenance
}

func flattenNodeProvenance(node *valueNode, path string, root bool, provenance Provenance) {
	if node == nil {
		return
	}
	if !root {
		provenance[path] = node.source
	}

	for key, child := range node.mapping {
		flattenNodeProvenance(child, appendProvenanceKey(path, key), false, provenance)
	}
	for index, child := range node.sequence {
		flattenNodeProvenance(child, fmt.Sprintf("%s[%d]", path, index), false, provenance)
	}
}

func appendProvenanceKey(parent, key string) string {
	if safeProvenanceSegment.MatchString(key) {
		if parent == "" {
			return key
		}
		return parent + "." + key
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		panic("encode provenance mapping key")
	}
	return parent + "[" + string(encoded) + "]"
}

func nodeFromAny(value any, source FieldSource) (*valueNode, error) {
	if value == nil {
		return &valueNode{kind: nodeNull, source: source}, nil
	}
	if number, ok := value.(json.Number); ok {
		if !jsonNumberPattern.MatchString(string(number)) {
			return nil, fmt.Errorf("configuration value type json.Number contains an invalid JSON number")
		}
		return &valueNode{kind: nodeScalar, scalar: number, source: source}, nil
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Bool:
		return &valueNode{kind: nodeScalar, scalar: reflected.Bool(), source: source}, nil
	case reflect.String:
		return &valueNode{kind: nodeScalar, scalar: reflected.String(), source: source}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number := json.Number(strconv.FormatInt(reflected.Int(), 10))
		return &valueNode{kind: nodeScalar, scalar: number, source: source}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		number := json.Number(strconv.FormatUint(reflected.Uint(), 10))
		return &valueNode{kind: nodeScalar, scalar: number, source: source}, nil
	case reflect.Map:
		if reflected.IsNil() {
			return &valueNode{kind: nodeNull, source: source}, nil
		}
		if reflected.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("configuration value type %s has non-string map keys", reflected.Type())
		}
		mapping := make(map[string]*valueNode, reflected.Len())
		iterator := reflected.MapRange()
		for iterator.Next() {
			child, err := nodeFromAny(iterator.Value().Interface(), source)
			if err != nil {
				return nil, err
			}
			mapping[iterator.Key().String()] = child
		}
		return &valueNode{kind: nodeMapping, mapping: mapping, source: source}, nil
	case reflect.Slice:
		if reflected.IsNil() {
			return &valueNode{kind: nodeNull, source: source}, nil
		}
		fallthrough
	case reflect.Array:
		sequence := make([]*valueNode, reflected.Len())
		for index := range reflected.Len() {
			child, err := nodeFromAny(reflected.Index(index).Interface(), source)
			if err != nil {
				return nil, err
			}
			sequence[index] = child
		}
		return &valueNode{kind: nodeSequence, sequence: sequence, source: source}, nil
	default:
		return nil, fmt.Errorf("configuration value type %s is unsupported", reflected.Type())
	}
}

func mustNodeFromAny(value any, source FieldSource) *valueNode {
	node, err := nodeFromAny(value, source)
	if err != nil {
		panic(fmt.Sprintf("invalid builtin configuration: %v", err))
	}
	return node
}
