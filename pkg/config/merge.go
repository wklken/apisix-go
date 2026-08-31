package config

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/json"
)

var jsonNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

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
	merged.pathBase = upper.pathBase
	for key, incoming := range upper.mapping {
		merged.mapping[key] = mergeNodes(merged.mapping[key], incoming)
	}
	return merged
}

func nodeFromAny(value any, pathBase string) (*valueNode, error) {
	if value == nil {
		return &valueNode{kind: nodeNull, pathBase: pathBase}, nil
	}
	if number, ok := value.(json.Number); ok {
		if !jsonNumberPattern.MatchString(string(number)) {
			return nil, fmt.Errorf("configuration value type json.Number contains an invalid JSON number")
		}
		return &valueNode{kind: nodeScalar, scalar: number, pathBase: pathBase}, nil
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Bool:
		return &valueNode{kind: nodeScalar, scalar: reflected.Bool(), pathBase: pathBase}, nil
	case reflect.String:
		if !utf8.ValidString(reflected.String()) {
			return nil, fmt.Errorf("configuration value type %s contains invalid UTF-8", reflected.Type())
		}
		return &valueNode{kind: nodeScalar, scalar: reflected.String(), pathBase: pathBase}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number := json.Number(strconv.FormatInt(reflected.Int(), 10))
		return &valueNode{kind: nodeScalar, scalar: number, pathBase: pathBase}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		number := json.Number(strconv.FormatUint(reflected.Uint(), 10))
		return &valueNode{kind: nodeScalar, scalar: number, pathBase: pathBase}, nil
	case reflect.Map:
		if reflected.IsNil() {
			return &valueNode{kind: nodeNull, pathBase: pathBase}, nil
		}
		if reflected.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("configuration value type %s has non-string map keys", reflected.Type())
		}
		mapping := make(map[string]*valueNode, reflected.Len())
		iterator := reflected.MapRange()
		for iterator.Next() {
			key := iterator.Key().String()
			if !utf8.ValidString(key) {
				return nil, fmt.Errorf(
					"configuration value type %s contains an invalid UTF-8 map key", reflected.Type(),
				)
			}
			child, err := nodeFromAny(iterator.Value().Interface(), pathBase)
			if err != nil {
				return nil, err
			}
			mapping[key] = child
		}
		return &valueNode{kind: nodeMapping, mapping: mapping, pathBase: pathBase}, nil
	case reflect.Slice:
		if reflected.IsNil() {
			return &valueNode{kind: nodeNull, pathBase: pathBase}, nil
		}
		fallthrough
	case reflect.Array:
		sequence := make([]*valueNode, reflected.Len())
		for index := range reflected.Len() {
			child, err := nodeFromAny(reflected.Index(index).Interface(), pathBase)
			if err != nil {
				return nil, err
			}
			sequence[index] = child
		}
		return &valueNode{kind: nodeSequence, sequence: sequence, pathBase: pathBase}, nil
	default:
		return nil, fmt.Errorf("configuration value type %s is unsupported", reflected.Type())
	}
}

func mustNodeFromAny(value any, pathBase string) *valueNode {
	node, err := nodeFromAny(value, pathBase)
	if err != nil {
		panic(fmt.Sprintf("invalid builtin configuration: %v", err))
	}
	return node
}
