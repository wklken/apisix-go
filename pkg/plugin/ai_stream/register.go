package ai_stream

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

// RegisterStreamingLLMRequestVars publishes the shared streaming request
// variables for single- and multi-instance AI proxying.
func RegisterStreamingLLMRequestVars(r *http.Request, requestDocument ai_protocols.Document, usage Usage) {
	if ctx.GetRequestVars(r) == nil {
		return
	}
	ctx.RegisterRequestVar(r, "$request_type", "ai_stream")
	if model := requestDocument.Model(); model != "" {
		ctx.RegisterRequestVar(r, "$request_llm_model", model)
	}
	if usage.Model != "" {
		ctx.RegisterRequestVar(r, "$llm_model", usage.Model)
	}
	if usage.PromptTokens >= 0 {
		ctx.RegisterRequestVar(r, "$llm_prompt_tokens", usage.PromptTokens)
	}
	if usage.CompletionTokens >= 0 {
		ctx.RegisterRequestVar(r, "$llm_completion_tokens", usage.CompletionTokens)
	}
	if len(usage.Raw) > 0 {
		ctx.RegisterRequestVar(r, "$llm_raw_usage", usage.Raw)
	}
	if usage.Text != "" {
		ctx.RegisterRequestVar(r, "$llm_response_text", usage.Text)
	}
	if usage.PromptTokens >= 0 && usage.CompletionTokens >= 0 {
		ctx.RegisterRequestVar(r, "$ai_token_usage", map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
		})
	}
}
