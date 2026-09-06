package compiler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestParityCompilerGenerationRetainsGraphQLQuota(t *testing.T) {
	sharedResources := runtime.NewResourceRegistry()
	first, firstFixture := newEffectiveBindingMaterializerFixture(
		t, []string{"graphql-limit-count"}, nil,
	)
	second, secondFixture := newEffectiveBindingMaterializerFixture(
		t, []string{"graphql-limit-count"}, nil,
	)
	first.registry, second.registry = sharedResources, sharedResources

	materialize := func(
		prepared *PreparedGeneration,
		fixture *effectiveBindingMaterializerFixture,
	) plugin.Binding {
		t.Helper()
		state, err := prepared.acquireHTTPRateLimitState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		spec := fixture.pluginSpec("graphql-limit-count", "route-1")
		spec.config = map[string]any{"count": 1, "time_window": 60}
		spec.runtimeContext = effectiveBindingRuntimeContext{
			configured:        true,
			enabledFactories:  []string{"graphql-limit-count"},
			publicAPIRegistry: public_api.NewRegistry(),
			runtimeAcquirer:   testTrafficSplitRuntimeAcquirer{},
			upstreamResolver: func(string) (resource.Upstream, error) {
				return resource.Upstream{}, nil
			},
			rateLimitState: state,
		}
		bindings, err := prepared.materializeEffectiveBindings(
			context.Background(), []effectiveBindingSpec{spec},
		)
		if err != nil || len(bindings) != 1 {
			t.Fatalf("materialize graphql-limit-count = (%#v, %v), want one binding", bindings, err)
		}
		return bindings[0]
	}

	request := func(binding plugin.Binding) int {
		t.Helper()
		r := httptest.NewRequest(
			http.MethodPost, "http://example.test/graphql", strings.NewReader("{viewer}"),
		)
		r.Header.Set("Content-Type", "application/graphql")
		w := httptest.NewRecorder()
		binding.Plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(w, r)
		return w.Code
	}

	firstBinding := materialize(first, firstFixture)
	if status := request(firstBinding); status != http.StatusNoContent {
		t.Fatalf("first generation status = %d, want %d", status, http.StatusNoContent)
	}
	secondBinding := materialize(second, secondFixture)
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := request(secondBinding); status != http.StatusServiceUnavailable {
		t.Fatalf(
			"successor generation status = %d, want %d retained quota",
			status,
			http.StatusServiceUnavailable,
		)
	}
}
