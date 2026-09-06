package degraphql

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestParityOmitAbsentVariables(t *testing.T) {
	p := newTestPlugin(t, Config{Query: `query ($name:String="world"){hello(name:$name)}`, Variables: []string{"name"}})
	for _, method := range []string{"POST", "GET"} {
		r := httptest.NewRequest(method, "http://example.com/graphql", strings.NewReader(`{}`))
		var vars map[string]any
		if method == "POST" {
			if err := p.rewritePOST(r); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			vars = payload["variables"].(map[string]any)
		} else {
			p.rewriteGET(r)
			if err := json.Unmarshal([]byte(r.URL.Query().Get("variables")), &vars); err != nil {
				t.Fatal(err)
			}
		}
		if value, exists := vars["name"]; exists {
			t.Errorf("%s emitted absent name=%#v; must omit to preserve GraphQL default", method, value)
		}
	}
}
