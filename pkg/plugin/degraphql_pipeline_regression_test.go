package plugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestDegraphqlTerminalStatusSurvivesResponsePipeline(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
	}{
		{"unsupported method", http.MethodPut, "", 405},
		{"empty body", http.MethodPost, "", 400},
		{"invalid body", http.MethodPost, "{", 400},
		{"valid body", http.MethodPost, `{"value":"ok"}`, 204},
		{"valid GET", http.MethodGet, "", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := parityBinding(
				t,
				"degraphql",
				`{"query":"query ($value: String!) { ping(value: $value) }","variables":["value"]}`,
				ScopeRoute,
			)
			bindings := []Binding{binding, parityBinding(t, "error-page", `{}`, ScopeRoute)}
			plan, err := BuildResponsePlan(bindings)
			if err != nil {
				t.Fatal(err)
			}
			pipeline := NewRequestPipeline(bindings, func(r *http.Request) (ConsumerResolution, error) {
				return ConsumerResolution{Request: r}, nil
			})
			called := false
			handler := plan.Install(pipeline, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
				w.WriteHeader(http.StatusNoContent)
			}))
			request, lifecycle := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(tc.method, "/graphql?value=ok", strings.NewReader(tc.body)),
				time.Now(),
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status || called != (tc.status == 204) {
				t.Fatalf(
					"response = %d %q, terminal called = %t; want %d",
					response.Code,
					response.Body.String(),
					called,
					tc.status,
				)
			}
			if tc.status != 204 && lifecycle.ResponseSource() != apisixctx.ResponseSourceEarlyStop {
				t.Fatalf("response source = %q; want early stop", lifecycle.ResponseSource())
			}
		})
	}
}
