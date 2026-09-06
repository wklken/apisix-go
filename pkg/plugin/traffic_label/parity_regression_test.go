package traffic_label

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityNoopActionContinuesRules(t *testing.T) {
	raw := []byte(`{"rules":[{"actions":[{"weight":1}]},{"actions":[{"set_headers":{"X-Label":"fallback"}}]}]}`)
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	if err := util.Validate(input, p.Schema); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, p.Config()); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Label"); got != "fallback" {
			t.Fatalf("got X-Label=%q; APISIX continues to fallback", got)
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}
