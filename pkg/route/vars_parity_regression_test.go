package route

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestParityRouteVarsSelectAndFallThrough(t *testing.T) {
	for _, tc := range []struct{ name, pattern string }{{"exact", "/users/alice"}, {"parameter", "/users/:name"}, {"prefix", "/users/*"}, {"embedded", "/users/*/details"}} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/users/alice"
			if tc.name == "embedded" {
				path += "/details"
			}
			routes := []PreparedRoute{
				{Route: resource.Route{ID: "fallback", Uri: tc.pattern}, Handler: varsTestHandler("fallback")},
				{
					Route: resource.Route{
						ID:       "conditional",
						Uri:      tc.pattern,
						Priority: 10,
						Vars:     json.RawMessage(`[["arg_age",">=",18],["http_x_role","==","member"]]`),
					},
					Handler: varsTestHandler("conditional"),
				},
			}
			snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: routes})
			if err != nil {
				t.Fatal(err)
			}
			// Mutating provider-owned expression bytes must not alter a detached snapshot.
			for i := range routes[1].Route.Vars {
				routes[1].Route.Vars[i] = ' '
			}
			for _, requestCase := range []struct{ query, role, want string }{{"?age=21", "member", "conditional"}, {"?age=17", "member", "fallback"}, {"?age=21", "guest", "fallback"}, {"", "member", "fallback"}} {
				request := httptest.NewRequest("GET", path+requestCase.query, nil)
				request.Header.Set("X-Role", requestCase.role)
				response := httptest.NewRecorder()
				snapshot.Handler().ServeHTTP(response, request)
				if response.Header().Get("Selected") != requestCase.want {
					t.Fatalf(
						"%s role=%s: status=%d selected=%q",
						request.URL,
						requestCase.role,
						response.Code,
						response.Header().Get("Selected"),
					)
				}
			}
		})
	}
}

func TestParityRouteVarsFallsThroughToOtherPathAndHost(t *testing.T) {
	for _, tc := range []struct{ name, preferred, fallback, host, fallbackHost string }{
		{"exact to prefix", "/users/alice", "/users/*", "", ""},
		{"parameter to prefix", "/users/:name", "/users/*", "", ""},
		{"long to short prefix", "/users/a*", "/users/*", "", ""},
		{"exact to parameter", "/users/alice", "/users/:name", "", ""},
		{"exact host to wildcard", "/users/alice", "/users/alice", "api.example.test", "*.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: []PreparedRoute{
				{
					Route:   resource.Route{ID: "fallback", Uri: tc.fallback, Host: tc.fallbackHost},
					Handler: varsTestHandler("fallback"),
				},
				{
					Route: resource.Route{
						ID:       "conditional",
						Uri:      tc.preferred,
						Host:     tc.host,
						Priority: 10,
						Vars:     json.RawMessage(`[["arg_select","==","yes"]]`),
					},
					Handler: varsTestHandler("conditional"),
				},
			}})
			if err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"?select=no", "?select=yes"} {
				response := httptest.NewRecorder()
				snapshot.Handler().
					ServeHTTP(response, httptest.NewRequest("GET", "http://api.example.test/users/alice"+query, nil))
				want := "fallback"
				if query == "?select=yes" {
					want = "conditional"
				}
				if response.Header().Get("Selected") != want {
					t.Fatalf("query=%s status=%d selected=%s", query, response.Code, response.Header().Get("Selected"))
				}
				if want == "fallback" && tc.fallback == "/users/:name" && response.Header().Get("Param") != "alice" {
					t.Fatalf("fallback route parameter=%q", response.Header().Get("Param"))
				}
			}
		})
	}
}

func TestParityRouteVarsMissingAndMalformed(t *testing.T) {
	for _, raw := range []string{`{}`, `[["arg_age","bad",18]]`, `[["arg_age",">="]]`} {
		if _, err := CompileHTTP(
			context.Background(),
			CompileInput{
				Revision: 1,
				Routes: []PreparedRoute{
					{
						Route:   resource.Route{ID: "invalid", Uri: "/", Vars: json.RawMessage(raw)},
						Handler: varsTestHandler("bad"),
					},
				},
			},
		); err == nil {
			t.Fatalf("accepted invalid vars: %s", raw)
		}
	}
	snapshot, err := CompileHTTP(
		context.Background(),
		CompileInput{
			Revision: 1,
			Routes: []PreparedRoute{
				{
					Route: resource.Route{
						ID:   "missing",
						Uri:  "/",
						Vars: json.RawMessage(`[["arg_token","==",null]]`),
					},
					Handler: varsTestHandler("missing"),
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		code int
	}{{"/", 204}, {"/?token=", 404}, {"/?token=x", 404}} {
		response := httptest.NewRecorder()
		snapshot.Handler().ServeHTTP(response, httptest.NewRequest("GET", tc.path, nil))
		if response.Code != tc.code {
			t.Fatalf("%s: status=%d want=%d", tc.path, response.Code, tc.code)
		}
	}
}

func varsTestHandler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Selected", name)
		w.Header().Set("Param", chi.URLParam(r, "name"))
		w.WriteHeader(204)
	})
}

func TestParityRouteVarsResolveBodyAndPathFamilies(t *testing.T) {
	for _, tc := range []struct{ name, method, target, contentType, body, vars string }{
		{"form", "POST", "/users/alice", "application/x-www-form-urlencoded", "name=alice&name=bob", `[["post_arg_name","==","alice"]]`},
		{"form nested syntax", "POST", "/users/alice", "application/x-www-form-urlencoded", "name=alice", `[["post_arg.name","==","alice"]]`},
		{"multipart", "POST", "/users/alice", "multipart/form-data; boundary=abc", "--abc\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\nalice\r\n--abc--\r\n", `[["post_arg.name","==","alice"]]`},
		{"http host", "GET", "http://api.example.test/users/alice", "", "", `[["http_host","==","api.example.test"]]`},
		{"http host with port", "GET", "http://api.example.test:9080/users/alice", "", "", `[["http_host","==","api.example.test:9080"]]`},
		{"path", "GET", "/users/alice", "", "", `[["uri_param_name","==","alice"]]`},
		{"json", "POST", "/users/alice", "application/json", `{"user":{"name":"alice"}}`, `[["post_arg.user.name","==","alice"]]`},
		{"json array", "POST", "/users/alice", "application/json", `{"users":[{"name":"alice"},{"name":"bob"}]}`, `[["post_arg.users[*].name","has","alice"]]`},
		{"graphql raw", "POST", "/users/alice", "", "query repo { owner { name } }", `[["graphql_name","==","repo"],["graphql_operation","==","query"],["graphql_root_fields","has","owner"]]`},
		{"graphql json first operation", "POST", "/users/alice", "application/json", `{"query":"query repo { owner { name } } query second { users { id } }","operationName":"second"}`, `[["graphql_name","==","repo"]]`},
		{"graphql get", "GET", "/users/alice?query=query%20repo%20%7Bowner%7Bname%7D%7D", "", "", `[["graphql_name","==","repo"]]`},
		{"missing json is nil", "POST", "/users/alice", "application/json", `{}`, `[["post_arg.missing","==",null]]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conditional := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != tc.body {
					t.Errorf("request body after vars = %q/%v, want %q", body, err, tc.body)
				}
				w.Header().Set("Selected", "conditional")
				w.Header().Set("Param", chi.URLParam(r, "name"))
				w.WriteHeader(204)
			})
			snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: []PreparedRoute{
				{Route: resource.Route{ID: "fallback", Uri: "/users/:other"}, Handler: varsTestHandler("fallback")},
				{
					Route: resource.Route{
						ID:       "conditional",
						Uri:      "/users/:name",
						Priority: 10,
						Vars:     json.RawMessage(tc.vars),
					},
					Handler: conditional,
				},
			}})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			snapshot.Handler().ServeHTTP(response, request)
			if response.Header().Get("Selected") != "conditional" || response.Header().Get("Param") != "alice" {
				t.Fatalf(
					"status=%d selected=%q param=%q",
					response.Code,
					response.Header().Get("Selected"),
					response.Header().Get("Param"),
				)
			}
		})
	}
}

func TestParityRouteGraphQLVarsPreserveBodyOnFallback(t *testing.T) {
	for _, body := range []string{"invalid GraphQL", "query repo { owner { name } }" + strings.Repeat(" ", 256)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			cfg := &appconfig.Config{GraphQL: appconfig.GraphQL{MaxSize: 24}}
			snapshot, err := CompileHTTP(context.Background(), CompileInput{
				Revision:                  1,
				StaticConfig:              cfg,
				PublicAPIRegistry:         public_api.NewRegistry(),
				GraphQLProxyCacheRegistry: graphql_proxy_cache.NewRegistry(),
				Routes: []PreparedRoute{
					{
						Route: resource.Route{ID: "fallback", Uri: "/graphql"},
						Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							got, err := io.ReadAll(r.Body)
							if err != nil || string(got) != body {
								t.Errorf("fallback body = %q/%v, want %q", got, err, body)
							}
							w.Header().Set("Selected", "fallback")
							w.WriteHeader(204)
						}),
					},
					{
						Route: resource.Route{
							ID:       "graphql",
							Uri:      "/graphql",
							Priority: 10,
							Vars:     json.RawMessage(`[["graphql_name","==","repo"]]`),
						},
						Handler: varsTestHandler("graphql"),
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			// The detached snapshot retains the configured bound.
			cfg.GraphQL.MaxSize = 1024
			response := httptest.NewRecorder()
			snapshot.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/graphql", strings.NewReader(body)))
			if response.Header().Get("Selected") != "fallback" {
				t.Fatalf("selected=%q", response.Header().Get("Selected"))
			}
		})
	}
}
