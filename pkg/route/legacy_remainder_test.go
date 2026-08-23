package route

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin"
)

type legacyRemainderTestPlugin struct {
	name  string
	order *[]string
}

func (p *legacyRemainderTestPlugin) Init() error               { return nil }
func (p *legacyRemainderTestPlugin) PostInit() error           { return nil }
func (p *legacyRemainderTestPlugin) Config() any               { return nil }
func (p *legacyRemainderTestPlugin) GetSchema() string         { return "" }
func (p *legacyRemainderTestPlugin) GetMetadataSchema() string { return "" }
func (p *legacyRemainderTestPlugin) GetPriority() int          { return 200 }
func (p *legacyRemainderTestPlugin) GetName() string           { return p.name }
func (p *legacyRemainderTestPlugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*p.order = append(*p.order, p.name+":enter")
		next.ServeHTTP(w, r)
		*p.order = append(*p.order, p.name+":exit")
	})
}

func TestLegacyRemainderPreservesEnterAndUnwindAroundScopedRewrite(t *testing.T) {
	order := []string{}
	rewrite := &scopedRewriteTestPlugin{name: "rewrite", priority: 1, order: &order}
	legacy := &legacyRemainderTestPlugin{name: "legacy", order: &order}
	handler := assembleRouteExecutor(
		[]plugin.Binding{
			bindScopedTestPlugin(
				"request-id",
				rewrite,
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-rewrite"},
			),
			bindScopedTestPlugin(
				"response-rewrite",
				legacy,
				plugin.ScopeRoute,
				plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-legacy"},
			),
		},
		nil,
		nil,
	).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		order = append(order, "upstream")
		w.WriteHeader(http.StatusNoContent)
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/legacy", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	want := []string{"legacy:enter", "rewrite", "upstream", "legacy:exit"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("execution order = %#v, want %#v", order, want)
	}
}
