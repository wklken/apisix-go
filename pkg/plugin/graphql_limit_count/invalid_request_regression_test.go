package graphql_limit_count

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInvalidGraphQLRequestsUseJSONMessageEnvelope(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body, message string }{
		{"empty body", "application/graphql", "", "Invalid graphql request: can't get graphql request body"},
		{"fragment only", "application/graphql", "fragment Fields on User { id }", "Invalid graphql request: empty graphql query"},
		{"invalid syntax", "application/graphql", "query {", "Invalid graphql request: failed to parse graphql query"},
		{"missing query", "application/json", `{}`, "invalid graphql request, json body[query] is nil"},
		{"empty query", "application/json", `{"query":""}`, "Invalid graphql request: empty graphql query"},
		{"wrong type", "text/plain", "{viewer}", "invalid graphql request, error content-type: text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Count: 10, TimeWindow: 60})
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid request reached upstream") })).
				ServeHTTP(response, request)
			want := "{\"message\":\"" + tc.message + "\"}\n"
			if response.Code != 400 || response.Body.String() != want ||
				response.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf(
					"response = %d %q %q; want %q",
					response.Code,
					response.Header().Get("Content-Type"),
					response.Body.String(),
					want,
				)
			}
		})
	}
}

func TestUndefinedGraphQLFragmentContinuesAndChargesRemainingDepth(t *testing.T) {
	p := newTestPlugin(t, Config{Count: 3, TimeWindow: 60})
	for index, wantStatus := range []int{204, 503} {
		response := parityRequest(p, `{viewer {id ...Missing}}`)
		if response.Code != wantStatus {
			t.Fatalf("request %d status=%d body=%q want%d", index, response.Code, response.Body.String(), wantStatus)
		}
		if index == 0 && response.Header().Get("X-RateLimit-Remaining") != "1" {
			t.Fatalf("remaining=%q, want1", response.Header().Get("X-RateLimit-Remaining"))
		}
	}
}
