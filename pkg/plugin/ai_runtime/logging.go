package ai_runtime

import (
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

func RegisterLogging(
	r *http.Request,
	summaries bool,
	payloads bool,
	protocol ai_protocols.Protocol,
	requestBody []byte,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	if summaries {
		apisixctx.RegisterRequestVar(r, "$llm_summary", map[string]any{
			"request_model":          apisixctx.GetRequestVar(r, "$request_llm_model"),
			"model":                  apisixctx.GetRequestVar(r, "$llm_model"),
			"duration":               apisixctx.GetRequestVar(r, "$llm_time_to_first_token"),
			"prompt_tokens":          apisixctx.GetRequestVar(r, "$llm_prompt_tokens"),
			"completion_tokens":      apisixctx.GetRequestVar(r, "$llm_completion_tokens"),
			"upstream_response_time": apisixctx.GetRequestVar(r, "$apisix_upstream_response_time"),
		})
	}
	if !payloads {
		return
	}

	var decoded map[string]any
	_ = json.Unmarshal(requestBody, &decoded)
	apisixctx.RegisterRequestVar(r, "$llm_request", map[string]any{
		"messages": ai_protocols.RequestContent(protocol, decoded),
		"stream":   ai_protocols.IsStreaming(protocol, decoded),
	})
	apisixctx.RegisterRequestVar(r, "$llm_response", map[string]any{
		"content": apisixctx.GetRequestVar(r, "$llm_response_text"),
	})
}
