package ai_protocols

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

// StringValue returns value when it is a string and an empty string otherwise.
func StringValue(value any) string {
	result, _ := value.(string)
	return result
}

// NumericUsage converts a usage counter value. Fractional float64 values are
// rounded when round is true and truncated otherwise. Unsupported types return
// -1.
func NumericUsage(value any, round bool) int64 {
	switch typed := value.(type) {
	case float64:
		if round {
			return int64(math.Round(typed))
		}
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return -1
	}
}

// AppendProtocolEndpoint appends the protocol-specific path to an
// OpenAI-compatible endpoint.
func AppendProtocolEndpoint(endpoint string, protocol Protocol) (string, error) {
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

// RegisterLLMRequestVars publishes the shared request variables for
// single- and multi-instance AI proxying.
func RegisterLLMRequestVars(r *http.Request, requestDocument Document, protocol Protocol, responseDocument Document) {
	if ctx.GetRequestVars(r) == nil {
		return
	}

	responseMetadata := ExtractResponseMetadataDocument(protocol, responseDocument)
	if responseDocument.Raw != nil {
		ctx.RegisterRequestVar(
			r,
			"$llm_response_text",
			ExtractResponseText(protocol, responseDocument.Raw),
		)
	}

	ctx.RegisterRequestVar(r, "$request_type", protocol.RequestType)
	if requestModel := requestDocument.Model(); requestModel != "" {
		ctx.RegisterRequestVar(r, "$request_llm_model", requestModel)
	}
	if responseMetadata.Model != "" {
		ctx.RegisterRequestVar(r, "$llm_model", responseMetadata.Model)
	}
	if responseMetadata.PromptTokens >= 0 {
		ctx.RegisterRequestVar(r, "$llm_prompt_tokens", responseMetadata.PromptTokens)
	}
	if responseMetadata.CompletionTokens >= 0 {
		ctx.RegisterRequestVar(r, "$llm_completion_tokens", responseMetadata.CompletionTokens)
	}
	RegisterUsageContextDocumentVars(
		r,
		responseDocument,
		responseMetadata.PromptTokens,
		responseMetadata.CompletionTokens,
	)
}

// RegisterUsageContextDocumentVars publishes the raw usage and token usage
// variables from a response document.
func RegisterUsageContextDocumentVars(r *http.Request, document Document, promptTokens int64, completionTokens int64) {
	usage, _ := document.Raw["usage"].(map[string]any)
	if usage == nil {
		return
	}
	ctx.RegisterRequestVar(r, "$llm_raw_usage", usage)
	if promptTokens < 0 || completionTokens < 0 {
		return
	}
	ctx.RegisterRequestVar(r, "$ai_token_usage", map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	})
}
