package util

import (
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// Parse converts source (typically a decoded JSON map) into dest using typed
// field assignment instead of a marshal/unmarshal roundtrip. dest must be a
// non-nil pointer. Supported destination shapes are converted directly;
// anything else (embedded fields, custom unmarshalers, RawMessage, ...) keeps
// the roundtrip so behavior always matches encoding/json.

func Parse(source any, dest any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("parse destination must be a non-nil pointer")
	}
	target := rv.Elem()
	if parseSupported(target.Type()) {
		return parseValue(target, source)
	}
	j, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(j, dest)
}

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	rawMessageType      = reflect.TypeFor[json.RawMessage]()
	parseSupportCache   sync.Map // reflect.Type -> bool
	parseSupportCacheMu sync.Mutex
)

func parseSupported(t reflect.Type) bool {
	if cached, ok := parseSupportCache.Load(t); ok {
		return cached.(bool)
	}
	parseSupportCacheMu.Lock()
	defer parseSupportCacheMu.Unlock()
	if cached, ok := parseSupportCache.Load(t); ok {
		return cached.(bool)
	}
	supported := parseSupportedType(t, make(map[reflect.Type]bool))
	parseSupportCache.Store(t, supported)
	return supported
}

func parseSupportedType(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return true
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.Interface:
		return true
	case reflect.Pointer, reflect.Slice:
		return parseSupportedType(t.Elem(), seen)
	case reflect.Array:
		return parseSupportedType(t.Elem(), seen)
	case reflect.Map:
		return t.Key().Kind() == reflect.String && parseSupportedType(t.Elem(), seen)
	case reflect.Struct:
		if t == rawMessageType ||
			reflect.PointerTo(t).Implements(jsonUnmarshalerType) ||
			reflect.PointerTo(t).Implements(textUnmarshalerType) {
			return false
		}
		for field := range t.Fields() {
			if field.Anonymous || field.PkgPath != "" {
				return false
			}
			if tag := field.Tag.Get("json"); tag == "-" {
				continue
			}
			if !parseSupportedType(field.Type, seen) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func parseValue(dest reflect.Value, source any) error {
	if source == nil {
		switch dest.Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice:
			dest.Set(reflect.Zero(dest.Type()))
		}
		return nil
	}
	switch dest.Kind() {
	case reflect.Interface:
		normalized, err := normalizeParseValue(source)
		if err != nil {
			return err
		}
		dest.Set(reflect.ValueOf(normalized))
		return nil
	case reflect.Pointer:
		if dest.IsNil() {
			dest.Set(reflect.New(dest.Type().Elem()))
		}
		return parseValue(dest.Elem(), source)
	case reflect.Struct:
		object, ok := source.(map[string]any)
		if !ok {
			return parseTypeError(source, dest.Type())
		}
		return parseStructFields(dest, object)
	case reflect.Map:
		object, ok := source.(map[string]any)
		if !ok {
			return parseTypeError(source, dest.Type())
		}
		if dest.IsNil() {
			dest.Set(reflect.MakeMapWithSize(dest.Type(), len(object)))
		}
		keyType := dest.Type().Key()
		elemType := dest.Type().Elem()
		for key, value := range object {
			elem := reflect.New(elemType).Elem()
			if err := parseValue(elem, value); err != nil {
				return err
			}
			dest.SetMapIndex(reflect.ValueOf(key).Convert(keyType), elem)
		}
		return nil
	case reflect.Slice:
		items, ok := source.([]any)
		if !ok {
			return parseTypeError(source, dest.Type())
		}
		out := reflect.MakeSlice(dest.Type(), len(items), len(items))
		for i, item := range items {
			if err := parseValue(out.Index(i), item); err != nil {
				return err
			}
		}
		dest.Set(out)
		return nil
	case reflect.Array:
		items, ok := source.([]any)
		if !ok || len(items) != dest.Len() {
			return parseTypeError(source, dest.Type())
		}
		for i, item := range items {
			if err := parseValue(dest.Index(i), item); err != nil {
				return err
			}
		}
		return nil
	default:
		return parseScalar(dest, source)
	}
}

type parseFieldInfo struct {
	index     int
	jsonName  string
	fieldName string
}

var parseFieldsCache sync.Map // reflect.Type -> []parseFieldInfo

func parseFieldsForType(t reflect.Type) []parseFieldInfo {
	if cached, ok := parseFieldsCache.Load(t); ok {
		return cached.([]parseFieldInfo)
	}
	fields := make([]parseFieldInfo, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = field.Name
		}
		fields = append(fields, parseFieldInfo{
			index:     i,
			jsonName:  name,
			fieldName: field.Name,
		})
	}
	parseFieldsCache.Store(t, fields)
	return fields
}

func parseStructFields(dest reflect.Value, object map[string]any) error {
	for _, field := range parseFieldsForType(dest.Type()) {
		value, ok := lookupJSONField(object, field.jsonName, field.fieldName)
		if !ok {
			continue
		}
		if err := parseValue(dest.Field(field.index), value); err != nil {
			return err
		}
	}
	return nil
}

func lookupJSONField(object map[string]any, jsonName, fieldName string) (any, bool) {
	if value, ok := object[jsonName]; ok {
		return value, true
	}
	if jsonName != fieldName {
		if value, ok := object[fieldName]; ok {
			return value, true
		}
	}
	for key, value := range object {
		if strings.EqualFold(key, jsonName) {
			return value, true
		}
	}
	return nil, false
}

func parseScalar(dest reflect.Value, source any) error {
	switch dest.Kind() {
	case reflect.String:
		value, ok := source.(string)
		if !ok {
			return parseTypeError(source, dest.Type())
		}
		dest.SetString(value)
		return nil
	case reflect.Bool:
		value, ok := source.(bool)
		if !ok {
			return parseTypeError(source, dest.Type())
		}
		dest.SetBool(value)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value, err := parseSignedNumber(source, dest.Type())
		if err != nil {
			return err
		}
		if dest.OverflowInt(value) {
			return parseTypeError(source, dest.Type())
		}
		dest.SetInt(value)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value, err := parseUnsignedNumber(source, dest.Type())
		if err != nil {
			return err
		}
		if dest.OverflowUint(value) {
			return parseTypeError(source, dest.Type())
		}
		dest.SetUint(value)
		return nil
	case reflect.Float32, reflect.Float64:
		value, err := parseFloatNumber(source, dest.Type())
		if err != nil {
			return err
		}
		if dest.OverflowFloat(value) {
			return parseTypeError(source, dest.Type())
		}
		dest.SetFloat(value)
		return nil
	default:
		return parseTypeError(source, dest.Type())
	}
}

func parseSignedNumber(source any, target reflect.Type) (int64, error) {
	switch value := source.(type) {
	case json.Number:
		// encoding/json parses the literal for integer targets.
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return 0, parseTypeError(source, target)
		}
		return parsed, nil
	case float64:
		if math.Trunc(value) != value {
			return 0, parseTypeError(source, target)
		}
		return int64(value), nil
	case float32:
		if math.Trunc(float64(value)) != float64(value) {
			return 0, parseTypeError(source, target)
		}
		return int64(value), nil
	case int:
		return int64(value), nil
	case int64:
		return value, nil
	case int32:
		return int64(value), nil
	case int16:
		return int64(value), nil
	case int8:
		return int64(value), nil
	case uint64:
		if value > math.MaxInt64 {
			return 0, parseTypeError(source, target)
		}
		return int64(value), nil
	case uint:
		return int64(value), nil
	case uint32:
		return int64(value), nil
	case uint16:
		return int64(value), nil
	case uint8:
		return int64(value), nil
	default:
		return 0, parseTypeError(source, target)
	}
}

func parseUnsignedNumber(source any, target reflect.Type) (uint64, error) {
	switch value := source.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(string(value), 10, 64)
		if err != nil {
			return 0, parseTypeError(source, target)
		}
		return parsed, nil
	case float64:
		if math.Trunc(value) != value || value < 0 {
			return 0, parseTypeError(source, target)
		}
		return uint64(value), nil
	case float32:
		if math.Trunc(float64(value)) != float64(value) || value < 0 {
			return 0, parseTypeError(source, target)
		}
		return uint64(value), nil
	case int:
		if value < 0 {
			return 0, parseTypeError(source, target)
		}
		return uint64(value), nil
	case int64:
		if value < 0 {
			return 0, parseTypeError(source, target)
		}
		return uint64(value), nil
	case int32:
		return uint64(value), nil
	case int16:
		return uint64(value), nil
	case int8:
		return uint64(value), nil
	case uint64:
		return value, nil
	case uint:
		return uint64(value), nil
	case uint32:
		return uint64(value), nil
	case uint16:
		return uint64(value), nil
	case uint8:
		return uint64(value), nil
	default:
		return 0, parseTypeError(source, target)
	}
}

func parseFloatNumber(source any, target reflect.Type) (float64, error) {
	switch value := source.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return 0, parseTypeError(source, target)
		}
		return parsed, nil
	case float64:
		return value, nil
	case float32:
		return float64(value), nil
	case int:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case int32:
		return float64(value), nil
	case int16:
		return float64(value), nil
	case int8:
		return float64(value), nil
	case uint64:
		return float64(value), nil
	case uint:
		return float64(value), nil
	case uint32:
		return float64(value), nil
	case uint16:
		return float64(value), nil
	case uint8:
		return float64(value), nil
	default:
		return 0, parseTypeError(source, target)
	}
}

// normalizeParseValue converts a decoded JSON value to the shapes encoding/json
// produces when unmarshaling into interface{}: numbers become float64, nested
// maps and slices are converted recursively.
func normalizeParseValue(source any) (any, error) {
	switch value := source.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return nil, fmt.Errorf("json: invalid number literal %q", string(value))
		}
		return parsed, nil
	case float64, float32, int, int64, int32, int16, int8, uint64, uint, uint32, uint16, uint8:
		return reflect.ValueOf(value).Convert(reflect.TypeFor[float64]()).Float(), nil
	case []any:
		items := make([]any, len(value))
		for i, item := range value {
			normalized, err := normalizeParseValue(item)
			if err != nil {
				return nil, err
			}
			items[i] = normalized
		}
		return items, nil
	case map[string]any:
		object := make(map[string]any, len(value))
		for key, item := range value {
			normalized, err := normalizeParseValue(item)
			if err != nil {
				return nil, err
			}
			object[key] = normalized
		}
		return object, nil
	default:
		return source, nil
	}
}

func parseTypeError(source any, target reflect.Type) error {
	return fmt.Errorf("json: cannot unmarshal %s into Go value of type %s", jsonTypeName(source), target)
}

func jsonTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case json.Number, float64, float32, int, int64, int32, int16, int8, uint64, uint, uint32, uint16, uint8:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}
