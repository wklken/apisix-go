package route

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestRouteVariablesUseRawNginxArgumentsAndJoinedHeaders(t *testing.T) {
	for _, test := range []struct {
		name, query, rule string
		headers           []string
		want              int
	}{
		{"bare flag", "?k", `[["arg_k","==",""]]`, nil, 404},
		{"empty argument", "?k=", `[["arg_k","==",""]]`, nil, 204},
		{"flag then value", "?k&k=v", `[["arg_k","==","v"]]`, nil, 204},
		{"semicolon", "?a=1;b=2", `[["arg_a","==","1;b=2"]]`, nil, 204},
		{"raw escapes", "?k=a%20b+c", `[["arg_k","==","a%20b+c"]]`, nil, 204},
		{"case insensitive", "?K=v", `[["arg_k","==","v"]]`, nil, 204},
		{"first equals", "?k=one&k=two", `[["arg_k","==","one"]]`, nil, 204},
		{"JSON null remains nil", "", `[["arg_k","==",null]]`, nil, 204},
		{"YAML null table is not nil", "", `[["arg_k","==",{}]]`, nil, 404},
		{"joined headers", "", `[["http_x_role","==","one, two"]]`, []string{"one", "two"}, 204},
		{"comma without space", "", `[["http_x_role","==","one,two"]]`, []string{"one", "two"}, 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := CompileHTTP(
				context.Background(),
				CompileInput{
					Revision: 1,
					Routes: []PreparedRoute{
						{
							Route:   resource.Route{ID: "vars", Uri: "/", Vars: json.RawMessage(test.rule)},
							Handler: varsTestHandler("selected"),
						},
					},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("GET", "/"+test.query, nil)
			for _, value := range test.headers {
				request.Header.Add("X-Role", value)
			}
			response := httptest.NewRecorder()
			snapshot.Handler().ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d", response.Code, test.want)
			}
		})
	}
}
