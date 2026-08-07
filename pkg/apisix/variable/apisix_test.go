package variable

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestGetApisixVarResolvesMatchedURIRouteID(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/orders/42", nil)
	routeContext := chi.NewRouteContext()
	routeContext.RoutePatterns = []string{"/orders/{id}"}
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))

	if got := GetApisixVar(request, "$matched_uri"); got != "/orders/{id}" {
		t.Fatalf("$matched_uri = %q, want /orders/{id}", got)
	}

	request = ctx.WithApisixVars(request, map[string]string{"$route_id": "route-1"})
	if got := GetApisixVar(request, "$route_id"); got != "route-1" {
		t.Fatalf("$route_id = %q, want route-1", got)
	}
	if got := GetApisixVar(request, "$service_name"); got != "" {
		t.Fatalf("$service_name = %q, want empty when absent", got)
	}
}

func TestGetApisixVarHandlesMissingRouteContext(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/orders/42", nil)
	if got := GetApisixVar(request, "$matched_uri"); got != "" {
		t.Fatalf("$matched_uri = %q, want empty without route context", got)
	}
}
