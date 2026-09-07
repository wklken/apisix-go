package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/authz_casbin"
)

func TestRegressionCasbinUsesNormalizedServingPath(t *testing.T) {
	const model = `[request_definition]
r = sub, obj, act
[policy_definition]
p = sub, obj, act
[policy_effect]
e = some(where (p.eft == allow))
[matchers]
m = r.sub == p.sub && keyMatch(r.obj, p.obj) && r.act == p.act
`
	p := &authz_casbin.Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	*p.Config().(*authz_casbin.Config) = authz_casbin.Config{
		Model:    model,
		Policy:   "p, alice, /public/*, GET",
		Username: "X-User",
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	var servedPath string
	handler := normalizeRequestPath(
		p.Handler(
			http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) { servedPath = r.URL.Path; w.WriteHeader(204) },
			),
		),
	)
	req := httptest.NewRequest("GET", "/public/../admin", nil)
	req.Header.Set("X-User", "alice")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf(
			"status=%d servedPath=%q; policy only permits /public/* and normalized request targets /admin",
			rr.Code,
			servedPath,
		)
	}
}
