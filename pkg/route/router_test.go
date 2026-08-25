package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestNormalizeRouteOrderEqualPriorityKeepsDeterministicRegistrationTie(t *testing.T) {
	t.Parallel()

	routes := []resource.Route{
		{ID: "equal-first", Uri: "/items/*", Priority: 5},
		{ID: "equal-second", Uri: "/items/*", Priority: 5},
	}
	normalized := normalizeRouteOrder(routes)
	if normalized[0].ID != "equal-first" || normalized[1].ID != "equal-second" {
		t.Fatalf("equal-priority order = [%s %s], want stable input order", normalized[0].ID, normalized[1].ID)
	}

	router := chi.NewRouter()
	registrar := newRouteRegistrar(router)
	statuses := []int{http.StatusCreated, http.StatusAccepted}
	for index, routeResource := range normalized {
		status := statuses[index]
		handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
		})
		if err := registrar.registerRouteWithHosts(nil, routeResource.Uri, nil, handler); err != nil {
			t.Fatalf("register route %q: %v", routeResource.ID, err)
		}
	}

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/items/42", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d from later normalized equal-priority route", response.Code, http.StatusAccepted)
	}
}
