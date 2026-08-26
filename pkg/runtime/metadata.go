package runtime

import (
	"bytes"
	"errors"
	"io"
	"reflect"

	apisixjson "github.com/wklken/apisix-go/pkg/json"
)

var (
	errMetadataFactoryRequired = errors.New("metadata view: factory name is required")
	errMetadataTargetRequired  = errors.New("metadata view: decode target must be a non-nil pointer")
	errMetadataDocumentInvalid = errors.New("metadata view: document is invalid")
)

// MetadataView is an immutable, generation-local view of plugin metadata.
//
// The view stores compact canonical JSON bytes rather than decoded values so
// callers cannot mutate the view through a returned map or slice. The zero
// value is an empty view.
type MetadataView struct {
	documents map[string][]byte
}

// NewMetadataView validates and copies plugin metadata documents into an
// immutable view. Every document must contain exactly one JSON object.
func NewMetadataView(metadata map[string][]byte) (MetadataView, error) {
	if len(metadata) == 0 {
		return MetadataView{}, nil
	}

	documents := make(map[string][]byte, len(metadata))
	for factory, document := range metadata {
		if factory == "" {
			return MetadataView{}, errMetadataFactoryRequired
		}
		canonical, err := canonicalMetadataDocument(document)
		if err != nil {
			return MetadataView{}, err
		}
		documents[factory] = canonical
	}
	return MetadataView{documents: documents}, nil
}

// Decode copies the metadata document for factory into target. It returns
// false when the factory has no metadata. A successful decode never exposes
// the view's internal bytes.
func (view MetadataView) Decode(factory string, target any) (bool, error) {
	if factory == "" {
		return false, errMetadataFactoryRequired
	}
	document, ok := view.documents[factory]
	if !ok {
		return false, nil
	}
	if !validMetadataTarget(target) {
		return false, errMetadataTargetRequired
	}

	decoder := apisixjson.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return false, errMetadataDocumentInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return false, errMetadataDocumentInvalid
	}
	return true, nil
}

func canonicalMetadataDocument(document []byte) ([]byte, error) {
	decoder := apisixjson.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errMetadataDocumentInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errMetadataDocumentInvalid
	}
	canonical, err := apisixjson.Marshal(object)
	if err != nil {
		return nil, errMetadataDocumentInvalid
	}
	return canonical, nil
}

func validMetadataTarget(target any) bool {
	if target == nil {
		return false
	}
	value := reflect.ValueOf(target)
	return value.Kind() == reflect.Pointer && !value.IsNil()
}
