package file_logger

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// fileLoggerEncoder keeps the production zap envelope while avoiding zap.Any's
// reflection path for the maps and arrays produced by the file logger.
type fileLoggerEncoder struct {
	encoder zapcore.Encoder
}

func newFileLoggerEncoder() *fileLoggerEncoder {
	config := zap.NewProductionConfig()
	config.DisableCaller = true
	config.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	return &fileLoggerEncoder{
		encoder: zapcore.NewJSONEncoder(config.EncoderConfig),
	}
}

func (e *fileLoggerEncoder) encode(fields map[string]any) (*buffer.Buffer, error) {
	return e.encoder.EncodeEntry(
		zapcore.Entry{
			Level:   zap.InfoLevel,
			Time:    time.Now(),
			Message: "",
		},
		[]zap.Field{zap.Inline(fileObjectMarshaler{values: fields})},
	)
}

type fileObjectMarshaler struct {
	values map[string]any
	nested bool
}

func (m fileObjectMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	for key, value := range m.values {
		if err := addFileLoggerValue(enc, key, value, m.nested); err != nil {
			return err
		}
	}
	return nil
}

type fileAnyArray struct {
	values []any
	nested bool
}

func (a fileAnyArray) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	for _, value := range a.values {
		if err := appendFileLoggerValue(enc, value, a.nested); err != nil {
			return err
		}
	}
	return nil
}

type fileStringArray []string

func (a fileStringArray) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	for _, value := range a {
		enc.AppendString(value)
	}
	return nil
}

type fileStringSliceMap map[string][]string

func (m fileStringSliceMap) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	for key, value := range m {
		if value == nil {
			if err := enc.AddReflected(key, value); err != nil {
				return err
			}
			continue
		}
		if err := enc.AddArray(key, fileStringArray(value)); err != nil {
			return err
		}
	}
	return nil
}

func addFileLoggerValue(enc zapcore.ObjectEncoder, key string, value any, nested bool) error {
	if nested && useReflectedCompositeValue(value) {
		return enc.AddReflected(key, value)
	}
	switch typed := value.(type) {
	case nil:
		return enc.AddReflected(key, nil)
	case zapcore.ObjectMarshaler:
		return enc.AddObject(key, typed)
	case zapcore.ArrayMarshaler:
		return enc.AddArray(key, typed)
	case string:
		enc.AddString(key, typed)
	case bool:
		enc.AddBool(key, typed)
	case int:
		enc.AddInt(key, typed)
	case int8:
		enc.AddInt8(key, typed)
	case int16:
		enc.AddInt16(key, typed)
	case int32:
		enc.AddInt32(key, typed)
	case int64:
		enc.AddInt64(key, typed)
	case uint:
		enc.AddUint(key, typed)
	case uint8:
		enc.AddUint8(key, typed)
	case uint16:
		enc.AddUint16(key, typed)
	case uint32:
		enc.AddUint32(key, typed)
	case uint64:
		enc.AddUint64(key, typed)
	case uintptr:
		enc.AddUintptr(key, typed)
	case float32:
		enc.AddFloat32(key, typed)
	case float64:
		enc.AddFloat64(key, typed)
	case complex64:
		enc.AddComplex64(key, typed)
	case complex128:
		enc.AddComplex128(key, typed)
	case time.Time:
		enc.AddTime(key, typed)
	case time.Duration:
		enc.AddDuration(key, typed)
	case error:
		if typed == nil {
			return enc.AddReflected(key, nil)
		}
		zap.NamedError(key, typed).AddTo(enc)
	case []byte:
		enc.AddBinary(key, typed)
	case []string:
		if nested && typed == nil {
			return enc.AddReflected(key, typed)
		}
		return enc.AddArray(key, fileStringArray(typed))
	case []any:
		if typed == nil {
			return enc.AddReflected(key, typed)
		}
		return enc.AddArray(key, fileAnyArray{values: typed, nested: true})
	case map[string]string:
		if typed == nil {
			return enc.AddReflected(key, typed)
		}
		return enc.AddObject(key, fileObjectMarshaler{values: stringMapToAny(typed), nested: true})
	case map[string][]string:
		if typed == nil {
			return enc.AddReflected(key, typed)
		}
		return enc.AddObject(key, fileStringSliceMap(typed))
	case map[string]any:
		if typed == nil {
			return enc.AddReflected(key, typed)
		}
		return enc.AddObject(key, fileObjectMarshaler{values: typed, nested: true})
	case fmt.Stringer:
		zap.Stringer(key, typed).AddTo(enc)
	default:
		return enc.AddReflected(key, value)
	}
	return nil
}

func appendFileLoggerValue(enc zapcore.ArrayEncoder, value any, nested bool) error {
	if nested && useReflectedCompositeValue(value) {
		return enc.AppendReflected(value)
	}
	switch typed := value.(type) {
	case nil:
		return enc.AppendReflected(nil)
	case zapcore.ObjectMarshaler:
		return enc.AppendObject(typed)
	case zapcore.ArrayMarshaler:
		return enc.AppendArray(typed)
	case string:
		enc.AppendString(typed)
	case bool:
		enc.AppendBool(typed)
	case int:
		enc.AppendInt(typed)
	case int8:
		enc.AppendInt8(typed)
	case int16:
		enc.AppendInt16(typed)
	case int32:
		enc.AppendInt32(typed)
	case int64:
		enc.AppendInt64(typed)
	case uint:
		enc.AppendUint(typed)
	case uint8:
		enc.AppendUint8(typed)
	case uint16:
		enc.AppendUint16(typed)
	case uint32:
		enc.AppendUint32(typed)
	case uint64:
		enc.AppendUint64(typed)
	case uintptr:
		enc.AppendUintptr(typed)
	case float32:
		enc.AppendFloat32(typed)
	case float64:
		enc.AppendFloat64(typed)
	case complex64:
		enc.AppendComplex64(typed)
	case complex128:
		enc.AppendComplex128(typed)
	case time.Time:
		enc.AppendTime(typed)
	case time.Duration:
		enc.AppendDuration(typed)
	case error:
		if typed == nil {
			return enc.AppendReflected(nil)
		}
		enc.AppendString(typed.Error())
	case []byte:
		return enc.AppendReflected(typed)
	case []string:
		if nested && typed == nil {
			return enc.AppendReflected(typed)
		}
		return enc.AppendArray(fileStringArray(typed))
	case []any:
		if typed == nil {
			return enc.AppendReflected(typed)
		}
		return enc.AppendArray(fileAnyArray{values: typed, nested: true})
	case map[string]string:
		if typed == nil {
			return enc.AppendReflected(typed)
		}
		return enc.AppendObject(fileObjectMarshaler{values: stringMapToAny(typed), nested: true})
	case map[string][]string:
		if typed == nil {
			return enc.AppendReflected(typed)
		}
		return enc.AppendObject(fileStringSliceMap(typed))
	case map[string]any:
		if typed == nil {
			return enc.AppendReflected(typed)
		}
		return enc.AppendObject(fileObjectMarshaler{values: typed, nested: true})
	case fmt.Stringer:
		enc.AppendString(typed.String())
	default:
		return enc.AppendReflected(value)
	}
	return nil
}

func useReflectedCompositeValue(value any) bool {
	switch value.(type) {
	case time.Time, time.Duration, error, fmt.Stringer,
		complex64, complex128, []byte,
		zapcore.ObjectMarshaler, zapcore.ArrayMarshaler:
		return true
	default:
		return false
	}
}

func stringMapToAny(values map[string]string) map[string]any {
	converted := make(map[string]any, len(values))
	for key, value := range values {
		converted[key] = value
	}
	return converted
}
