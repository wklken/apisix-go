package variable

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

var RequestVars = map[string]struct{}{
	"$status": {},
}

func GetRequestVar(r *http.Request, key string) any {
	return ctx.GetRequestVar(r, key)
}
