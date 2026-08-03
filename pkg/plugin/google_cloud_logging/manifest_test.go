package google_cloud_logging

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStandaloneManifestMapsOneIndependentCasePerPinnedBlock(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "google-cloud-logging.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertGoogleCloudManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			File  string `yaml:"file"`
			Tests int    `yaml:"tests"`
		} `yaml:"sources"`
		Cases []struct {
			Name   string `yaml:"name"`
			Source struct {
				File  string `yaml:"file"`
				Tests []int  `yaml:"tests"`
			} `yaml:"source"`
			Config map[string]any `yaml:"config"`
			Input  struct {
				Path string `yaml:"path"`
			} `yaml:"input"`
			Steps []any `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	wantSources := []struct {
		file  string
		tests int
	}{
		{"t/plugin/google-cloud-logging.t", 25},
		{"t/plugin/google-cloud-logging2.t", 7},
		{"t/plugin/google-cloud-logging3.t", 1},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	for i, want := range wantSources {
		if got := manifest.Sources[i]; got.File != want.file || got.Tests != want.tests {
			t.Fatalf("source %d = (%q, %d), want (%q, %d)", i+1, got.File, got.Tests, want.file, want.tests)
		}
	}

	if len(manifest.Cases) != 33 {
		t.Fatalf("top-level cases = %d, want exactly 33 independent pinned cases", len(manifest.Cases))
	}
	next := map[string]int{
		"t/plugin/google-cloud-logging.t":  1,
		"t/plugin/google-cloud-logging2.t": 1,
		"t/plugin/google-cloud-logging3.t": 1,
	}
	for i, testCase := range manifest.Cases {
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf("case %d %q source tests = %v, want one pinned block", i+1, testCase.Name, testCase.Source.Tests)
		}
		want, ok := next[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unexpected source %q", i+1, testCase.Name, testCase.Source.File)
		}
		if got := testCase.Source.Tests[0]; got != want {
			t.Fatalf("case %d %q source test = %d, want %d", i+1, testCase.Name, got, want)
		}
		next[testCase.Source.File]++
		if len(testCase.Config) == 0 {
			t.Errorf("case %d %q has no standalone config", i+1, testCase.Name)
		}
		if !containsGoogleCloudLoggingConfig(testCase.Config) {
			t.Errorf("case %d %q does not configure google-cloud-logging", i+1, testCase.Name)
		}
		if testCase.Input.Path == "" && len(testCase.Steps) == 0 {
			t.Errorf("case %d %q has no real request", i+1, testCase.Name)
		}
	}
	for _, want := range wantSources {
		if got := next[want.file] - 1; got != want.tests {
			t.Fatalf("%s mapped through block %d, want %d", want.file, got, want.tests)
		}
	}
}

func containsGoogleCloudLoggingConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "google-cloud-logging" || containsGoogleCloudLoggingConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsGoogleCloudLoggingConfig)
	}
	return false
}

func assertGoogleCloudManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertGoogleCloudManifestHasNoAliasesOrMerges(t, child)
	}
}
