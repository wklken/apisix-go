package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigTestValidatesStandaloneYAML(t *testing.T) {
	for name, body := range map[string]string{"missing environment": "routes: [{uri: '${{FINAL_CONFIG_UNSET}}'}]", "invalid YAML": "routes: [", "valid without END": "routes: []"} {
		t.Run(name, func(t *testing.T) {
			cwd := isolatedCommandRoot(t)
			if err := os.WriteFile(filepath.Join(cwd, "conf/apisix.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			path := writeCommandConfig(t, "deployment: {role: data_plane, role_data_plane: {config_provider: yaml}}")
			root := newRootCommand()
			root.SetArgs([]string{"config", "test", "-c", path})
			err := root.Execute()
			if name == "valid without END" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid standalone YAML was accepted")
			}
		})
	}
}
