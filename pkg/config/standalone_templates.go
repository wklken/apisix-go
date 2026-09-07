package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/wklken/apisix-go/pkg/json"
)

func expandStandaloneTemplates(data []byte, provider string) ([]byte, error) {
	if !bytes.Contains(data, []byte("${{")) {
		return data, nil
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if found {
			environment[key] = value
		}
	}
	if provider == standaloneProviderYAML {
		// APISIX substitutes raw YAML so quoting determines the expanded type.
		expanded, err := expandAPISIXTemplates(string(data), "standalone", environment)
		return []byte(expanded.value), err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse standalone JSON: invalid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("parse standalone JSON: invalid trailing content")
	}
	document, err := expandStandaloneJSONValue(document, environment)
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func expandStandaloneJSONValue(value any, environment map[string]string) (any, error) {
	switch value := value.(type) {
	case string:
		expanded, err := expandAPISIXTemplates(value, "standalone", environment)
		if err != nil {
			return nil, err
		}
		if expanded.expanded {
			return retypeExpandedScalar(expanded.value), nil
		}
		return value, nil
	case []any:
		for index, element := range value {
			expanded, err := expandStandaloneJSONValue(element, environment)
			if err != nil {
				return nil, err
			}
			value[index] = expanded
		}
		return value, nil
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make(map[string]any, len(value))
		for _, key := range keys {
			expandedKey, err := expandAPISIXTemplates(key, "standalone", environment)
			if err != nil {
				return nil, err
			}
			expanded, err := expandStandaloneJSONValue(value[key], environment)
			if err != nil {
				return nil, err
			}
			result[expandedKey.value] = expanded
		}
		return result, nil
	default:
		return value, nil
	}
}

// ValidateStandaloneYAMLFile mirrors the APISIX CLI precheck. Missing files are
// optional here; initial publication checks resource schemas and the END marker.
func ValidateStandaloneYAMLFile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read standalone YAML: %w", err)
	}
	data, err = expandStandaloneTemplates(data, standaloneProviderYAML)
	if err != nil {
		return err
	}
	var document any
	if err := yaml.Unmarshal(data, &document); err != nil || document == nil {
		return fmt.Errorf("invalid standalone YAML configuration")
	}
	return nil
}
