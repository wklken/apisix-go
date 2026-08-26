package runtime

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"

	apisixjson "github.com/wklken/apisix-go/pkg/json"
)

var (
	errMetadataFactoryRequired = errors.New("metadata view: factory name is required")
	errMetadataTargetRequired  = errors.New("metadata view: decode target must be a non-nil pointer")
	errMetadataDocumentInvalid = errors.New("metadata view: document is invalid")
	errMetadataViewClosed      = errors.New("metadata view: authority is closed")
)

type MetadataSecret interface {
	Use(func(string) error) error
}

type MetadataDocument struct {
	Document []byte
	Secrets  map[string]MetadataSecret
}

type metadataDocument struct {
	document []byte
	secrets  map[string]MetadataSecret
}

type metadataViewState struct {
	mu        sync.RWMutex
	closed    bool
	documents map[string]metadataDocument
}

// MetadataView is an immutable, generation-local view of plugin metadata.
// Copies share revocation state. A factory-scoped copy cannot observe sibling
// metadata, and the zero value is an empty view.
type MetadataView struct {
	state   *metadataViewState
	factory string
}

func NewMetadataView(metadata map[string][]byte) (MetadataView, error) {
	if len(metadata) == 0 {
		return MetadataView{}, nil
	}
	documents := make(map[string]MetadataDocument, len(metadata))
	for factory, document := range metadata {
		documents[factory] = MetadataDocument{Document: document}
	}
	return NewMetadataViewWithSecrets(documents)
}

// NewMetadataViewWithSecrets keeps private values outside the shared
// canonical JSON and injects them only into a factory-owned decode copy.
func NewMetadataViewWithSecrets(metadata map[string]MetadataDocument) (MetadataView, error) {
	if len(metadata) == 0 {
		return MetadataView{}, nil
	}
	documents := make(map[string]metadataDocument, len(metadata))
	for factory, document := range metadata {
		if factory == "" {
			return MetadataView{}, errMetadataFactoryRequired
		}
		canonical, err := canonicalMetadataDocument(document.Document)
		if err != nil {
			return MetadataView{}, err
		}
		secrets := make(map[string]MetadataSecret, len(document.Secrets))
		for pointer, value := range document.Secrets {
			if pointer == "" || value == nil {
				return MetadataView{}, errMetadataDocumentInvalid
			}
			secrets[pointer] = value
		}
		documents[factory] = metadataDocument{document: canonical, secrets: secrets}
	}
	return MetadataView{state: &metadataViewState{documents: documents}}, nil
}

func (view MetadataView) ForFactory(factory string) MetadataView {
	return MetadataView{state: view.state, factory: factory}
}

func (view MetadataView) Close() {
	if view.state == nil {
		return
	}
	view.state.mu.Lock()
	view.state.closed = true
	view.state.documents = nil
	view.state.mu.Unlock()
}

func (view MetadataView) Decode(factory string, target any) (bool, error) {
	if factory == "" {
		return false, errMetadataFactoryRequired
	}
	if view.factory != "" && view.factory != factory {
		return false, nil
	}
	if !validMetadataTarget(target) {
		return false, errMetadataTargetRequired
	}
	if view.state == nil {
		return false, nil
	}

	view.state.mu.RLock()
	defer view.state.mu.RUnlock()
	if view.state.closed {
		return false, errMetadataViewClosed
	}
	document, ok := view.state.documents[factory]
	if !ok {
		return false, nil
	}

	decoder := apisixjson.NewDecoder(bytes.NewReader(document.document))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return false, errMetadataDocumentInvalid
	}
	for pointer, value := range document.secrets {
		if err := value.Use(func(plaintext string) error {
			return setMetadataJSONPointer(object, pointer, plaintext)
		}); err != nil {
			return false, errMetadataDocumentInvalid
		}
	}
	materialized, err := apisixjson.Marshal(object)
	if err != nil {
		return false, errMetadataDocumentInvalid
	}
	defer clear(materialized)

	decoder = apisixjson.NewDecoder(bytes.NewReader(materialized))
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

func setMetadataJSONPointer(document map[string]any, pointer string, replacement string) error {
	if !strings.HasPrefix(pointer, "/") {
		return errMetadataDocumentInvalid
	}
	segments := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	var current any = document
	for index, encoded := range segments {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		terminal := index == len(segments)-1
		switch value := current.(type) {
		case map[string]any:
			child, exists := value[segment]
			if !exists {
				return errMetadataDocumentInvalid
			}
			if terminal {
				value[segment] = replacement
				return nil
			}
			current = child
		case []any:
			position, err := strconv.Atoi(segment)
			if err != nil || position < 0 || position >= len(value) {
				return errMetadataDocumentInvalid
			}
			if terminal {
				value[position] = replacement
				return nil
			}
			current = value[position]
		default:
			return errMetadataDocumentInvalid
		}
	}
	return errMetadataDocumentInvalid
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
