package route

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestRouteMatchesParametersWithTerminalWildcards(t *testing.T) {
	for _, test := range []struct{ pattern, path, user, action string }{
		{"/user/:user/*action", "/user/john/", "john", ""},
		{"/user/:user/*action", "/user/john/send", "john", "send"},
		{"/user/:user/*action", "/user/j/send/to/other", "j", "send/to/other"},
		{"/user/:user/*", "/user/john/send", "john", "send"},
		{"/user/*action", "/user/send", "", "send"},
		{"/user/:user/*action/comments", "/user/j/send/to/comments", "j", "send/to"},
		{"/user/:user/*action/comments", "/user/j//comments", "j", ""},
		{"/user/:user/*/comments", "/user/j/send/to/comments", "j", "send/to"},
		{"/user/:user/*/comments", "/user/j//comments", "j", ""},
		{"/user/*action/comments", "/user/send/to/comments", "", "send/to"},
	} {
		t.Run(test.pattern+test.path, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("User", chi.URLParam(r, "user"))
				action := chi.URLParam(r, "action")
				if action == "" {
					action = chi.URLParam(r, "*")
				}
				w.Header().Set("Action", action)
				w.WriteHeader(204)
			})
			route := resource.Route{ID: "wildcard", Uri: test.pattern}
			actionVariable := "uri_param_action"
			if strings.HasSuffix(test.pattern, "*") || strings.Contains(test.pattern, "/*/") {
				actionVariable = "uri_param_:ext"
			}
			conditions := []any{[]any{actionVariable, "==", test.action}}
			if test.user != "" {
				conditions = append(conditions, []any{"uri_param_user", "==", test.user})
			}
			raw, err := json.Marshal(conditions)
			if err != nil {
				t.Fatal(err)
			}
			route.Vars = raw
			snapshot, err := CompileHTTP(
				context.Background(),
				CompileInput{Revision: 1, Routes: []PreparedRoute{{Route: route, Handler: handler}}},
			)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			snapshot.Handler().ServeHTTP(response, httptest.NewRequest("GET", test.path, nil))
			if response.Code != 204 || response.Header().Get("User") != test.user ||
				response.Header().Get("Action") != test.action {
				t.Fatalf("response=%d headers=%v", response.Code, response.Header())
			}
		})
	}
}

func TestMixedEmbeddedWildcardFallsBackWhenSuffixDoesNotMatch(t *testing.T) {
	snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: []PreparedRoute{
		{
			Route:   resource.Route{ID: "fallback", Uri: "/user/*"},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(202) }),
		},
		{
			Route:   resource.Route{ID: "embedded", Uri: "/user/:user/*action/comments"},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/user/j/send/comments", 204}, {"/user/j//comments", 204}, {"/user/j/send/other", 202}, {"/user/j/comments", 202},
	} {
		response := httptest.NewRecorder()
		snapshot.Handler().ServeHTTP(response, httptest.NewRequest("GET", tc.path, nil))
		if response.Code != tc.want {
			t.Errorf("path=%s status=%d want=%d", tc.path, response.Code, tc.want)
		}
	}
}

func TestEmbeddedWildcardOrderUsesPriorityThenPathLength(t *testing.T) {
	for _, prefix := range []string{"/foo/*action", "/foo/*", "/foo/:name/*action"} {
		for _, reversed := range []bool{false, true} {
			for _, shortPriority := range []int{0, 1} {
				t.Run(
					fmt.Sprintf("%s/reverse=%t/short-priority=%d", prefix, reversed, shortPriority),
					func(t *testing.T) {
						routes := []PreparedRoute{
							{
								Route: resource.Route{ID: "long", Uri: prefix + "/x/bar"},
								Handler: http.HandlerFunc(
									func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Picked", "long"); w.WriteHeader(204) },
								),
							},
							{
								Route: resource.Route{ID: "short", Uri: prefix + "/bar", Priority: shortPriority},
								Handler: http.HandlerFunc(
									func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Picked", "short"); w.WriteHeader(204) },
								),
							},
						}
						if reversed {
							routes[0], routes[1] = routes[1], routes[0]
						}
						snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: routes})
						if err != nil {
							t.Fatal(err)
						}
						response := httptest.NewRecorder()
						snapshot.Handler().
							ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/foo/a/b/x/bar", nil))
						want := "long"
						if shortPriority > 0 {
							want = "short"
						}
						if response.Code != 204 || response.Header().Get("Picked") != want {
							t.Fatalf(
								"response=%d picked=%s want=%s",
								response.Code,
								response.Header().Get("Picked"),
								want,
							)
						}
					},
				)
			}
		}
	}
}
