package json

import (
	"io"

	gojson "github.com/goccy/go-json"
)

type (
	RawMessage  = gojson.RawMessage
	Number      = gojson.Number
	Unmarshaler = gojson.Unmarshaler
)

type Decoder interface {
	Decode(any) error
	UseNumber()
	DisallowUnknownFields()
}

type Encoder interface {
	Encode(any) error
	SetEscapeHTML(bool)
	SetIndent(string, string)
}

func Marshal(value any) ([]byte, error) {
	return gojson.Marshal(value)
}

func Unmarshal(data []byte, value any) error {
	return gojson.Unmarshal(data, value)
}

func MarshalIndent(value any, prefix, indent string) ([]byte, error) {
	return gojson.MarshalIndent(value, prefix, indent)
}

func NewDecoder(reader io.Reader) Decoder {
	return gojson.NewDecoder(reader)
}

func NewEncoder(writer io.Writer) Encoder {
	return gojson.NewEncoder(writer)
}
