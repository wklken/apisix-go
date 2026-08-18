package file_logger

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

func TestFileLoggerEncoderIncludesProductionEnvelopeAndTypedValues(t *testing.T) {
	when := time.Date(2026, time.August, 17, 12, 34, 56, 0, time.UTC)
	errValue := errors.New("boom")
	entry := map[string]any{
		"nil":        nil,
		"string":     "value",
		"bool":       true,
		"int":        int(-1),
		"int8":       int8(-2),
		"int16":      int16(-3),
		"int32":      int32(-4),
		"int64":      int64(-5),
		"uint":       uint(1),
		"uint8":      uint8(2),
		"uint16":     uint16(3),
		"uint32":     uint32(4),
		"uint64":     uint64(5),
		"float32":    float32(1.5),
		"float64":    float64(2.5),
		"complex64":  complex64(1 + 2i),
		"complex128": complex128(3 + 4i),
		"time":       when,
		"duration":   1500 * time.Millisecond,
		"error":      errValue,
		"strings":    []string{"a", "b"},
		"any":        []any{"x", 2, false},
		"string_map": map[string]string{
			"key": "value",
		},
		"string_slice_map": map[string][]string{
			"key": {"a", "b"},
		},
		"nested": map[string]any{
			"value": []any{map[string]any{"ok": true}},
		},
		"object": testObjectMarshaler{},
		"array":  testArrayMarshaler{},
	}

	encoded, err := newFileLoggerEncoder().encode(entry)
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}
	defer encoded.Free()

	var decoded map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON decode %q: %v", encoded.String(), err)
	}
	if decoded["level"] != "info" {
		t.Fatalf("level = %#v, want info", decoded["level"])
	}
	if _, ok := decoded["ts"].(float64); !ok {
		t.Fatalf("ts = %#v, want numeric production timestamp", decoded["ts"])
	}
	if got := decoded["msg"]; got != "" {
		t.Fatalf("msg = %#v, want empty production message", got)
	}
	if got := decoded["string"]; got != "value" {
		t.Fatalf("string = %#v, want value", got)
	}
	if got := decoded["duration"]; got != 1.5 {
		t.Fatalf("duration = %#v, want seconds", got)
	}
	if got := decoded["error"]; got != "boom" {
		t.Fatalf("error = %#v, want boom", got)
	}
	if got := decoded["object"].(map[string]any)["object"]; got != "ok" {
		t.Fatalf("object = %#v, want ok", got)
	}
	if got := decoded["array"].([]any); len(got) != 2 || got[0] != "first" || got[1] != float64(2) {
		t.Fatalf("array = %#v, want typed array", got)
	}
}

func TestFileLoggerEncoderUsesFallbackOnlyForUncommonValues(t *testing.T) {
	type uncommon struct {
		Value string
	}
	encoded, err := newFileLoggerEncoder().encode(map[string]any{
		"uncommon": uncommon{Value: "fallback"},
	})
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}
	defer encoded.Free()

	var decoded map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON decode %q: %v", encoded.String(), err)
	}
	if got := decoded["uncommon"].(map[string]any)["Value"]; got != "fallback" {
		t.Fatalf("uncommon = %#v, want reflected fallback", got)
	}
}

func TestFileLoggerEncoderSupportsZapMarshalerContracts(t *testing.T) {
	encoded, err := newFileLoggerEncoder().encode(map[string]any{
		"object": testObjectMarshaler{},
		"array":  testArrayMarshaler{},
	})
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}
	defer encoded.Free()

	var decoded map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON decode %q: %v", encoded.String(), err)
	}
	if decoded["object"].(map[string]any)["object"] != "ok" {
		t.Fatalf("object = %#v, want custom marshaler value", decoded["object"])
	}
	if got := decoded["array"].([]any); len(got) != 2 {
		t.Fatalf("array = %#v, want two values", got)
	}
}

func TestFileLoggerEncoderPreservesLegacyNestedCompositeSemantics(t *testing.T) {
	when := time.Date(2026, time.August, 17, 12, 34, 56, 0, time.UTC)
	fields := map[string]any{
		"nil_map":   map[string]any(nil),
		"nil_array": []any(nil),
		"nested": map[string]any{
			"time":     when,
			"duration": 1500 * time.Millisecond,
			"stringer": nestedStringer{Value: "field"},
			"array":    []any{when, 1500 * time.Millisecond, nestedStringer{Value: "array"}},
			"nil_list": []string(nil),
		},
	}
	legacy, err := encodeLegacyFileLoggerEntry(fields)
	if err != nil {
		t.Fatalf("legacy encode error = %v", err)
	}
	defer legacy.Free()
	current, err := newFileLoggerEncoder().encode(fields)
	if err != nil {
		t.Fatalf("current encode error = %v", err)
	}
	defer current.Free()

	var legacyObject, currentObject map[string]any
	if err := json.Unmarshal(legacy.Bytes(), &legacyObject); err != nil {
		t.Fatalf("decode legacy entry: %v", err)
	}
	if err := json.Unmarshal(current.Bytes(), &currentObject); err != nil {
		t.Fatalf("decode current entry: %v", err)
	}
	delete(legacyObject, "ts")
	delete(currentObject, "ts")
	if !reflect.DeepEqual(currentObject, legacyObject) {
		t.Fatalf("nested semantics changed:\nlegacy=%#v\ncurrent=%#v", legacyObject, currentObject)
	}
}

func TestFileLoggerEncoderPreservesGuardedTopLevelStringerSemantics(t *testing.T) {
	fields := map[string]any{
		"panic":     panicStringer{},
		"typed_nil": (*typedNilStringer)(nil),
	}
	legacy, err := encodeLegacyFileLoggerEntry(fields)
	if err != nil {
		t.Fatalf("legacy encode error = %v", err)
	}
	defer legacy.Free()
	current, err := newFileLoggerEncoder().encode(fields)
	if err != nil {
		t.Fatalf("current encode error = %v", err)
	}
	defer current.Free()

	var legacyObject, currentObject map[string]any
	if err := json.Unmarshal(legacy.Bytes(), &legacyObject); err != nil {
		t.Fatalf("decode legacy entry: %v", err)
	}
	if err := json.Unmarshal(current.Bytes(), &currentObject); err != nil {
		t.Fatalf("decode current entry: %v", err)
	}
	delete(legacyObject, "ts")
	delete(currentObject, "ts")
	if !reflect.DeepEqual(currentObject, legacyObject) {
		t.Fatalf("guarded Stringer semantics changed:\nlegacy=%#v\ncurrent=%#v", legacyObject, currentObject)
	}
}

func encodeLegacyFileLoggerEntry(fields map[string]any) (*buffer.Buffer, error) {
	config := zap.NewProductionConfig()
	config.DisableCaller = true
	encoder := zapcore.NewJSONEncoder(config.EncoderConfig)
	zapFields := make([]zap.Field, 0, len(fields))
	for key, value := range fields {
		zapFields = append(zapFields, zap.Any(key, value))
	}
	return encoder.EncodeEntry(zapcore.Entry{Level: zap.InfoLevel, Time: time.Now()}, zapFields)
}

type nestedStringer struct {
	Value string
}

func (value nestedStringer) String() string { return "string:" + value.Value }

type panicStringer struct{}

func (panicStringer) String() string { panic("stringer failed") }

type typedNilStringer struct {
	Value string
}

func (value *typedNilStringer) String() string { return value.Value }

type testObjectMarshaler struct{}

func (testObjectMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("object", "ok")
	return nil
}

type testArrayMarshaler struct{}

func (testArrayMarshaler) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	enc.AppendString("first")
	enc.AppendInt(2)
	return nil
}
