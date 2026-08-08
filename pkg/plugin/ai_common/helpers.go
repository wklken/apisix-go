// Package ai_common holds stateless helpers shared by the AI proxy plugins.
package ai_common

import (
	"net/http"
	"strings"
)

// CloneJSONValue deep-clones a JSON-decoded value.
func CloneJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = CloneJSONValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = CloneJSONValue(item)
		}
		return out
	default:
		return value
	}
}

// AsAnyMap casts a value to map[string]any.
func AsAnyMap(value any) (map[string]any, bool) {
	out, ok := value.(map[string]any)
	return out, ok
}

// MergeBodyMap merges override into dst, cloning override values. It reports
// whether any value in dst actually changed.
func MergeBodyMap(dst map[string]any, override map[string]any, force bool) bool {
	changed := false
	for key, overrideValue := range override {
		currentValue, exists := dst[key]
		currentMap, currentIsMap := AsAnyMap(currentValue)
		overrideMap, overrideIsMap := AsAnyMap(overrideValue)
		if exists && currentIsMap && overrideIsMap {
			if MergeBodyMap(currentMap, overrideMap, force) {
				changed = true
			}
			continue
		}
		if !exists || force {
			if !exists || !JSONValueEqual(currentValue, overrideValue) {
				changed = true
			}
			dst[key] = CloneJSONValue(overrideValue)
		}
	}
	return changed
}

// JSONValueEqual reports whether two JSON-decoded values are deeply equal
// without reflection or intermediate serialization.
func JSONValueEqual(left any, right any) bool {
	switch typed := left.(type) {
	case map[string]any:
		other, ok := right.(map[string]any)
		if !ok || len(typed) != len(other) {
			return false
		}
		for key, value := range typed {
			if !JSONValueEqual(value, other[key]) {
				return false
			}
		}
		return true
	case []any:
		other, ok := right.([]any)
		if !ok || len(typed) != len(other) {
			return false
		}
		for index, value := range typed {
			if !JSONValueEqual(value, other[index]) {
				return false
			}
		}
		return true
	default:
		switch right.(type) {
		case map[string]any, []any:
			return false
		}
		return left == right
	}
}

// CopyForwardHeaders copies request headers that are safe to forward to the
// provider, skipping hop-by-hop headers.
func CopyForwardHeaders(dst, src http.Header) {
	for field, values := range src {
		switch strings.ToLower(field) {
		case "host", "content-length", "accept-encoding":
			continue
		}
		for _, value := range values {
			dst.Add(field, value)
		}
	}
}

// ProviderUsesOpenAIChat reports whether the provider speaks the OpenAI chat
// protocol.
func ProviderUsesOpenAIChat(provider string) bool {
	switch provider {
	case "openai", "deepseek", "aimlapi", "openai-compatible", "azure-openai", "openrouter", "gemini",
		"vertex-ai":
		return true
	default:
		return false
	}
}
