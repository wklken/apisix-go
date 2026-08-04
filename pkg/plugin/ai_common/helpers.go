// Package ai_common holds stateless helpers shared by the AI proxy plugins.
package ai_common

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
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

// MergeBodyMap merges override into dst, cloning override values.
func MergeBodyMap(dst map[string]any, override map[string]any, force bool) {
	for key, overrideValue := range override {
		currentValue, exists := dst[key]
		currentMap, currentIsMap := AsAnyMap(currentValue)
		overrideMap, overrideIsMap := AsAnyMap(overrideValue)
		if exists && currentIsMap && overrideIsMap {
			MergeBodyMap(currentMap, overrideMap, force)
			continue
		}
		if !exists || force {
			dst[key] = CloneJSONValue(overrideValue)
		}
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

// AppendProtocolEndpoint appends the protocol-specific path to an
// OpenAI-compatible endpoint.
func AppendProtocolEndpoint(endpoint string, protocol ai_protocols.Protocol) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse OpenAI-compatible endpoint: %w", err)
	}
	switch strings.TrimRight(parsed.Path, "/") {
	case "", "/v1":
		parsed.Path = protocol.Endpoint
	default:
		return endpoint, nil
	}
	return parsed.String(), nil
}

// AppendBedrockEndpoint appends the model path to a Bedrock endpoint.
func AppendBedrockEndpoint(endpoint string, model string, streaming bool) (string, error) {
	if model == "" {
		return "", fmt.Errorf("bedrock requires options.model or request body model")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse Bedrock endpoint: %w", err)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return endpoint, nil
	}
	suffix := "/converse"
	if streaming {
		suffix = "/converse-stream"
	}
	parsed.Path = "/model/" + model + suffix
	parsed.RawPath = "/model/" + strings.ReplaceAll(url.QueryEscape(model), "+", "%20") + suffix
	return parsed.String(), nil
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
