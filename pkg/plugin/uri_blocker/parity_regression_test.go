package uri_blocker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityEmptyBlockRulesMatchEmptyRegex(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"block_rules": []any{}}
	if err := util.Validate(cfg, p.GetSchema()); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(cfg, p.Config()); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("request should match empty expression") })).
		ServeHTTP(response, httptest.NewRequest("GET", "/anything", nil))
	if response.Code != 403 {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestParitySchemaAdmitsRejectionCodeAbove999(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Validate(
		map[string]any{"block_rules": []any{"blocked"}, "rejected_code": 1000},
		p.GetSchema(),
	); err != nil {
		t.Fatal(err)
	}
}
