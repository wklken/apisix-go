package tencent_cloud_cls

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStandaloneManifestMapsOneIndependentCasePerPinnedBlock(t *testing.T) {
	const (
		sourceFile  = "t/plugin/tencent-cloud-cls.t"
		sourceTests = 22
	)
	path := filepath.Join("..", "..", "..", "t", "plugin", "tencent-cloud-cls.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertTencentCLSManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Source struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"source"`
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

	if manifest.Source.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("source commit = %q, want pinned Apache APISIX commit", manifest.Source.Commit)
	}
	if manifest.Source.File != sourceFile || manifest.Source.Tests != sourceTests {
		t.Fatalf(
			"source = (%q, %d tests), want (%q, %d tests)",
			manifest.Source.File,
			manifest.Source.Tests,
			sourceFile,
			sourceTests,
		)
	}
	if len(manifest.Cases) != sourceTests {
		t.Fatalf("top-level cases = %d, want exactly %d", len(manifest.Cases), sourceTests)
	}
	for i, testCase := range manifest.Cases {
		number := i + 1
		if testCase.Source.File != sourceFile {
			t.Errorf("case %d source file = %q, want %q", number, testCase.Source.File, sourceFile)
		}
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != number {
			t.Errorf("case %d source tests = %v, want [%d]", number, testCase.Source.Tests, number)
		}
		if len(testCase.Config) == 0 {
			t.Errorf("case %d %q has no standalone config", number, testCase.Name)
		}
		if !containsTencentCLSConfig(testCase.Config) {
			t.Errorf("case %d %q does not configure tencent-cloud-cls", number, testCase.Name)
		}
		if testCase.Input.Path == "" && len(testCase.Steps) == 0 {
			t.Errorf("case %d %q has no real request", number, testCase.Name)
		}
	}
}

func containsTencentCLSConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "tencent-cloud-cls" || containsTencentCLSConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsTencentCLSConfig)
	}
	return false
}

func assertTencentCLSManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertTencentCLSManifestHasNoAliasesOrMerges(t, child)
	}
}
