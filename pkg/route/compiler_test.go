package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestCompileHTTPDoesNotObserveInputMutation(t *testing.T) {
	t.Parallel()

	input := CompileInput{
		Revision: 7,
		Routes: []PreparedRoute{{
			Route: resource.Route{ID: "r1", Uri: "/before"},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}),
		}},
	}
	snapshot, err := CompileHTTP(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	input.Routes[0].Route.Uri = "/after"
	assertCompiledHTTPStatus(t, snapshot.Handler(), http.MethodGet, "/before", http.StatusNoContent)
	assertCompiledHTTPStatus(t, snapshot.Handler(), http.MethodGet, "/after", http.StatusNotFound)
}

func TestCompileHTTPRejectsIncompleteInput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		ctx   context.Context
		input CompileInput
	}{
		{name: "nil context", input: CompileInput{Revision: 1}},
		{name: "zero revision", ctx: context.Background()},
		{
			name: "missing route id", ctx: context.Background(),
			input: CompileInput{Revision: 1, Routes: []PreparedRoute{{
				Route: resource.Route{Uri: "/missing-id"}, Handler: http.NotFoundHandler(),
			}}},
		},
		{
			name: "nil handler", ctx: context.Background(),
			input: CompileInput{Revision: 1, Routes: []PreparedRoute{{
				Route: resource.Route{ID: "r1", Uri: "/nil"},
			}}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := CompileHTTP(test.ctx, test.input); err == nil {
				t.Fatal("CompileHTTP() error = nil")
			}
		})
	}
}

func assertCompiledHTTPStatus(
	t *testing.T,
	handler http.Handler,
	method string,
	path string,
	want int,
) {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s status = %d, want %d", method, path, response.Code, want)
	}
}
