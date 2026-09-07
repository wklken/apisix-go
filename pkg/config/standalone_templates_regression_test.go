package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestStandaloneExpandsFileTemplatesBeforeResourceNormalization(t *testing.T) {
	t.Setenv("FINAL_ROUTE", "/expanded")
	t.Setenv("FINAL_NUMBER", "42")
	t.Setenv("FINAL_ENABLED", "true")
	for _, provider := range []string{"yaml", "json"} {
		t.Run(provider, func(t *testing.T) {
			body := `routes:
  - id: env-route
    uri: '${{FINAL_ROUTE}}'
    labels:
      quoted: "${{FINAL_NUMBER}}"
      numeric: ${{FINAL_NUMBER}}
      enabled: ${{FINAL_ENABLED}}
      fallback: ${{FINAL_MISSING := fallback}}
#END
`
			if provider == "json" {
				body = `{"routes":[{"id":"env-route","uri":"${{FINAL_ROUTE}}","labels":{"quoted":"${{FINAL_NUMBER}}","numeric":"${{FINAL_NUMBER}}","enabled":"${{FINAL_ENABLED}}","fallback":"${{FINAL_MISSING := fallback}}"}}]}`
			}
			file := filepath.Join(t.TempDir(), "apisix."+provider)
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := readStandaloneSnapshot(file, provider, testStandaloneDataEncryption(t, false, nil))
			if err != nil {
				t.Fatal(err)
			}
			var route map[string]any
			if err := json.Unmarshal(snapshot["routes"]["env-route"], &route); err != nil {
				t.Fatal(err)
			}
			if route["uri"] != "/expanded" {
				t.Fatalf("URI=%v, want expanded route", route["uri"])
			}
			labels := route["labels"].(map[string]any)
			if labels["numeric"] != float64(42) || labels["enabled"] != true || labels["fallback"] != "fallback" {
				t.Fatalf("labels=%v", labels)
			}
			var quoted any = "42"
			if provider == "json" {
				quoted = float64(42)
			}
			if labels["quoted"] != quoted {
				t.Fatalf("quoted=%v (%T), want %v (%T)", labels["quoted"], labels["quoted"], quoted, quoted)
			}
		})
	}
}

func TestStandaloneMissingTemplateDoesNotPublishLiteral(t *testing.T) {
	for _, provider := range []string{"yaml", "json"} {
		t.Run(provider, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "apisix."+provider)
			body := "routes: [{id: env-route, uri: '${{FINAL_UNSET_ROUTE}}'}]\n#END\n"
			if provider == "json" {
				body = `{"routes":[{"id":"env-route","uri":"${{FINAL_UNSET_ROUTE}}"}]}`
			}
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := readStandaloneSnapshot(file, provider, testStandaloneDataEncryption(t, false, nil))
			if err == nil || !strings.Contains(err.Error(), "FINAL_UNSET_ROUTE") || snapshot != nil {
				t.Fatalf("snapshot=%v err=%v, want rejected missing environment", snapshot, err)
			}
		})
	}
}
