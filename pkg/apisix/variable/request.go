package variable

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

var RequestVars = map[string]struct{}{
	"$ai_request_body_changed":       {},
	"$ai_token_usage":                {},
	"$apisix_upstream_response_time": {},
	"$llm_completion_tokens":         {},
	"$llm_content_risk_level":        {},
	"$llm_model":                     {},
	"$llm_prompt_tokens":             {},
	"$llm_raw_usage":                 {},
	"$llm_request":                   {},
	"$llm_request_body":              {},
	"$llm_request_done":              {},
	"$llm_request_start_time":        {},
	"$llm_response":                  {},
	"$llm_response_text":             {},
	"$llm_summary":                   {},
	"$llm_time_to_first_token":       {},
	"$request_llm_model":             {},
	"$request_type":                  {},
	"$status":                        {},
}

func GetRequestVar(r *http.Request, key string) any {
	return ctx.GetRequestVar(r, key)
}
