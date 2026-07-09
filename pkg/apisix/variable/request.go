package variable

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

var RequestVars = map[string]struct{}{
	"$ai_request_body_changed": {},
	"$llm_completion_tokens":   {},
	"$llm_model":               {},
	"$llm_prompt_tokens":       {},
	"$llm_request_body":        {},
	"$llm_request_start_time":  {},
	"$request_llm_model":       {},
	"$request_type":            {},
	"$status":                  {},
}

func GetRequestVar(r *http.Request, key string) any {
	return ctx.GetRequestVar(r, key)
}
