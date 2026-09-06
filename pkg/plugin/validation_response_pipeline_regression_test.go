package plugin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestValidationRejectSurvivesResponseRewrite(t *testing.T) {
	spec := `{"openapi":"3.0.2","info":{"title":"test","version":"1"},"paths":{"/valid":{"get":{"responses":{"200":{"description":"OK"}}}}}}`
	for _, test := range []struct{ factory, config string }{
		{"request-validation", `{"header_schema":{"type":"object","required":["x-required"]}}`},
		{"oas-validator", fmt.Sprintf(`{"spec":%q}`, spec)},
	} {
		t.Run(test.factory, func(t *testing.T) {
			bindings := []Binding{
				parityBinding(t, test.factory, test.config, ScopeRoute),
				parityBinding(
					t,
					"response-rewrite",
					`{"body":"rewritten","headers":{"set":{"X-Rewritten":"1"}}}`,
					ScopeRoute,
				),
			}
			plan, err := BuildResponsePlan(bindings)
			if err != nil {
				t.Fatal(err)
			}
			handler := plan.Install(
				NewRequestPipeline(bindings, nil),
				http.HandlerFunc(
					func(http.ResponseWriter, *http.Request) { t.Fatal("invalid request reached upstream") },
				),
			)
			request, lifecycle := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(http.MethodGet, "/invalid", nil),
				time.Now(),
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 400 || response.Body.String() != "rewritten" ||
				response.Header().Get("X-Rewritten") != "1" ||
				lifecycle.ResponseSource() != apisixctx.ResponseSourceEarlyStop {
				t.Fatalf(
					"status=%d body=%q headers=%v source=%s",
					response.Code,
					response.Body.String(),
					response.Header(),
					lifecycle.ResponseSource(),
				)
			}
		})
	}
}
