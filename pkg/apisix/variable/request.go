package variable

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

var RequestVars = map[string]struct{}{
	"$ai_request_body_changed": {},
	"$llm_request_body":        {},
	"$llm_request_start_time":  {},
	"$status":                  {},
}

func GetRequestVar(r *http.Request, key string) any {
	return ctx.GetRequestVar(r, key)
}
