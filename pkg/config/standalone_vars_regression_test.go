package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestStandalonePreservesYAMLNullRouteExpressionConstant(t *testing.T) {
	for _, provider := range []string{"yaml", "json"} {
		t.Run(provider, func(t *testing.T) {
			body := "routes:\n  - id: vars\n    uri: /\n    vars: [[arg_k, '==', null]]\n#END\n"
			if provider == "json" {
				body = `{"routes":[{"id":"vars","uri":"/","vars":[["arg_k","==",null]]}]}`
			}
			file := filepath.Join(t.TempDir(), "apisix."+provider)
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := readStandaloneSnapshot(file, provider, testStandaloneDataEncryption(t, false, nil))
			if err != nil {
				t.Fatal(err)
			}
			var route struct {
				Vars [][]any `json:"vars"`
			}
			if err := json.Unmarshal(snapshot["routes"]["vars"], &route); err != nil {
				t.Fatal(err)
			}
			value := route.Vars[0][2]
			if provider == "json" {
				if value != nil {
					t.Fatalf("JSON null=%#v", value)
				}
			} else if table, ok := value.(map[string]any); !ok || len(table) != 0 {
				t.Fatalf("YAML null=%#v, want lyaml empty-table value", value)
			}
		})
	}
}
