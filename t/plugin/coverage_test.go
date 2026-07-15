package pluginintegration

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

var (
	documentedPluginName   = regexp.MustCompile("`([^`]+)`")
	upstreamSourceAbsences = map[string]string{
		"GM":              "no Apache APISIX t/plugin source at the pinned commit",
		"proxy-buffering": "no Apache APISIX t/plugin source at the pinned commit",
	}
)

func TestSupportedPluginManifestSelection(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "plugins.md"))
	if err != nil {
		t.Fatalf("read docs/plugins.md: %v", err)
	}

	plugins, err := supportedPluginNames(data)
	if err != nil {
		t.Fatalf("supportedPluginNames() error = %v", err)
	}
	if got := len(plugins); got != 100 {
		t.Fatalf("supported plugins = %d, want 100", got)
	}

	manifests := make(map[string]bool, len(plugins)-len(upstreamSourceAbsences))
	for _, pluginName := range plugins {
		if _, absent := upstreamSourceAbsences[pluginName]; !absent {
			manifests[pluginName] = true
		}
	}
	if problems := manifestCoverageProblems(plugins, manifests); len(problems) != 0 {
		t.Fatalf("complete manifest set problems = %v", problems)
	}

	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	actual := make(map[string]bool, len(files))
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		if name == "redirect2" {
			name = "redirect"
		}
		actual[name] = true
	}
	if problems := manifestCoverageProblems(plugins, actual); len(problems) != 0 {
		t.Fatalf("checked-in manifest set problems = %v", problems)
	}

	delete(manifests, "redirect")
	problems := manifestCoverageProblems(plugins, manifests)
	if len(problems) != 1 || !strings.Contains(problems[0], "redirect") {
		t.Fatalf("missing manifest problems = %v, want redirect", problems)
	}

	manifests["redirect"] = true
	manifests["not-a-plugin"] = true
	problems = manifestCoverageProblems(plugins, manifests)
	if len(problems) != 1 || !strings.Contains(problems[0], "not-a-plugin") {
		t.Fatalf("extra manifest problems = %v, want not-a-plugin", problems)
	}
}

func TestManifestCorpusValidates(t *testing.T) {
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no manifests found")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := loadManifest(file, data); err != nil {
			t.Fatalf("load %s: %v", file, err)
		}
	}
}

func TestManifestExercisesTargetPlugin(t *testing.T) {
	tests := []struct {
		name     string
		manifest *Manifest
		plugin   string
		want     bool
	}{
		{
			name: "route plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"acl": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   true,
		},
		{
			name: "global plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"global_rules": []any{map[string]any{"plugins": map[string]any{"error-page": map[string]any{}}}},
			}}}},
			plugin: "error-page",
			want:   true,
		},
		{
			name: "control plugin",
			manifest: &Manifest{Cases: []Case{{Runtime: map[string]any{
				"plugins": []any{"node-status"},
			}}}},
			plugin: "node-status",
			want:   true,
		},
		{
			name: "variant plugin",
			manifest: &Manifest{Cases: []Case{{Variants: []CaseVariant{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"ua-restriction": map[string]any{}}}},
			}}}}}},
			plugin: "ua-restriction",
			want:   true,
		},
		{
			name: "fixture proxy placeholder",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"uri": "/*", "upstream": map[string]any{}}},
			}}}},
			plugin: "saml-auth",
			want:   false,
		},
		{
			name: "unrelated plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"mocking": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := manifestExercisesPlugin(test.manifest, test.plugin); got != test.want {
				t.Fatalf("manifestExercisesPlugin() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestManifestCorpusExercisesTargetPlugins(t *testing.T) {
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	for _, file := range files {
		file := file
		pluginName := manifestPluginName(file)
		t.Run(pluginName, func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			manifest, err := loadManifest(file, data)
			if err != nil {
				t.Fatalf("load %s: %v", file, err)
			}
			assertManifestExercisesTargetPlugin(t, file, manifest, pluginName)
		})
	}
}

func manifestPluginName(file string) string {
	pluginName := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if pluginName == "redirect2" {
		return "redirect"
	}
	return pluginName
}

func assertManifestExercisesTargetPlugin(t *testing.T, file string, manifest *Manifest, pluginName string) {
	t.Helper()
	if !manifestExercisesPlugin(manifest, pluginName) {
		t.Errorf("%s never activates target plugin %q", file, pluginName)
	}
	for caseIndex := range manifest.Cases {
		caseSpec := &manifest.Cases[caseIndex]
		if len(caseSpec.Variants) == 0 {
			if !scenarioExercisesPlugin(caseSpec.Runtime, caseSpec.Config, pluginName) {
				t.Errorf("%s case %q never activates target plugin %q", file, caseSpec.Name, pluginName)
			}
			continue
		}
		for variantIndex := range caseSpec.Variants {
			variant := &caseSpec.Variants[variantIndex]
			if !scenarioExercisesPlugin(variant.Runtime, variant.Config, pluginName) {
				t.Errorf(
					"%s case %q variant %q never activates target plugin %q",
					file,
					caseSpec.Name,
					variant.Name,
					pluginName,
				)
			}
		}
	}
}

func manifestExercisesPlugin(manifest *Manifest, pluginName string) bool {
	for i := range manifest.Cases {
		caseSpec := &manifest.Cases[i]
		if scenarioExercisesPlugin(caseSpec.Runtime, caseSpec.Config, pluginName) {
			return true
		}
		for j := range caseSpec.Variants {
			variant := &caseSpec.Variants[j]
			if scenarioExercisesPlugin(variant.Runtime, variant.Config, pluginName) {
				return true
			}
		}
	}
	return false
}

func scenarioExercisesPlugin(runtime, config map[string]any, pluginName string) bool {
	switch plugins := runtime["plugins"].(type) {
	case []any:
		for _, configured := range plugins {
			if configured == pluginName {
				return true
			}
		}
	case []string:
		if slices.Contains(plugins, pluginName) {
			return true
		}
	}
	return configContainsPlugin(config, pluginName)
}

func configContainsPlugin(value any, pluginName string) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			if key == "plugins" {
				if plugins, ok := nested.(map[string]any); ok {
					if _, configured := plugins[pluginName]; configured {
						return true
					}
				}
			}
			if configContainsPlugin(nested, pluginName) {
				return true
			}
		}
	case []any:
		for _, nested := range current {
			if configContainsPlugin(nested, pluginName) {
				return true
			}
		}
	}
	return false
}

func supportedPluginNames(data []byte) ([]string, error) {
	var plugins []string
	seen := make(map[string]bool)
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "|---") {
			continue
		}
		fields := strings.Split(strings.Trim(line, "|"), "|")
		if len(fields) < 6 || strings.TrimSpace(fields[3]) != "yes" ||
			!strings.HasPrefix(strings.TrimSpace(fields[5]), "Supported") {
			continue
		}
		match := documentedPluginName.FindStringSubmatch(fields[1])
		if len(match) != 2 {
			return nil, fmt.Errorf("supported plugin row has no backtick name: %s", line)
		}
		if seen[match[1]] {
			return nil, fmt.Errorf("supported plugin %q is duplicated", match[1])
		}
		seen[match[1]] = true
		plugins = append(plugins, match[1])
	}
	if len(plugins) == 0 {
		return nil, fmt.Errorf("no supported plugin rows found")
	}
	return plugins, nil
}

func manifestCoverageProblems(plugins []string, manifests map[string]bool) []string {
	selected := make(map[string]bool, len(plugins))
	var problems []string
	for _, pluginName := range plugins {
		selected[pluginName] = true
		if _, absent := upstreamSourceAbsences[pluginName]; absent {
			if manifests[pluginName] {
				problems = append(problems, fmt.Sprintf("source-absence plugin %s has a manifest", pluginName))
			}
			continue
		}
		if !manifests[pluginName] {
			problems = append(problems, fmt.Sprintf("supported plugin %s has no manifest", pluginName))
		}
	}
	for pluginName := range upstreamSourceAbsences {
		if !selected[pluginName] {
			problems = append(problems, fmt.Sprintf("source-absence plugin %s is not selected", pluginName))
		}
	}
	for pluginName := range manifests {
		if !selected[pluginName] {
			problems = append(problems, fmt.Sprintf("manifest %s is not a supported plugin", pluginName))
		}
	}
	sort.Strings(problems)
	return problems
}
