package variable

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

var RequestVars = map[string]struct{}{
	"$status": {},
}

func GetRequestVar(r *http.Request, key string) any {
	if _, ok := RequestVars[key]; !ok {
		return ""
	}

	vars := ctx.GetRequestVars(r)
	if vars == nil {
		return ""
	}
	if val, ok := vars[key]; ok {
		return val
	}
	return ""
}
