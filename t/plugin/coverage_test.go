package pluginintegration

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var documentedPluginRow = regexp.MustCompile("`([^`]+)`")

func TestDocumentedPluginManifests(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "plugins.md"))
	if err != nil {
		t.Fatalf("read docs/plugins.md: %v", err)
	}

	noUpstreamSuite := map[string]string{
		"GM":                  "no matching upstream t/plugin/*.t file exists at the pinned commit",
		"proxy-cache":         "no matching upstream t/plugin/*.t file exists at the pinned commit",
		"graphql-proxy-cache": "no matching upstream t/plugin/*.t file exists at the pinned commit",
		"proxy-buffering":     "no matching upstream t/plugin/*.t file exists at the pinned commit",
	}
	seen := make(map[string]bool)
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Plugin |") || strings.HasPrefix(line, "|---") {
			continue
		}
		fields := strings.Split(strings.Trim(line, "|"), "|")
		if len(fields) < 6 || !strings.HasPrefix(strings.TrimSpace(fields[5]), "Supported") {
			continue
		}
		match := documentedPluginRow.FindStringSubmatch(fields[1])
		if len(match) != 2 {
			t.Fatalf("supported plugin row has no backtick name: %s", line)
		}
		pluginName := match[1]
		seen[pluginName] = true
		manifestPath := filepath.Join(pluginName + ".yaml")
		if reason, ok := noUpstreamSuite[pluginName]; ok {
			if _, err := os.Stat(manifestPath); err == nil {
				t.Errorf("%s has no upstream suite (%s), but %s exists", pluginName, reason, manifestPath)
			}
			continue
		}
		manifestData, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Errorf("supported plugin %s has no manifest: %v", pluginName, err)
			continue
		}
		if _, err := loadManifest(manifestPath, manifestData); err != nil {
			t.Errorf("load supported plugin %s manifest: %v", pluginName, err)
		}
	}

	for pluginName := range noUpstreamSuite {
		if !seen[pluginName] {
			t.Errorf("no-upstream exception %s is not a Supported row in docs/plugins.md", pluginName)
		}
	}
}
