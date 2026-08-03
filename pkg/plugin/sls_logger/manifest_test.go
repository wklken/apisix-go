package sls_logger

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStandaloneManifestMapsOneIndependentCasePerPinnedBlock(t *testing.T) {
	const sourceFile = "t/plugin/sls-logger.t"

	path := filepath.Join("..", "..", "..", "t", "plugin", "sls-logger.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertSLSManifestHasNoAliasesOrMerges(t, &document)
	for _, placeholder := range [][]byte{
		[]byte("source-complete skip"),
		[]byte("standalone fixture is available"),
		[]byte("not exercised"),
	} {
		if bytes.Contains(data, placeholder) {
			t.Fatalf("manifest contains placeholder text %q", placeholder)
		}
	}

	var manifest struct {
		Sources []struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"sources"`
		Cases []struct {
			Name   string `yaml:"name"`
			Source struct {
				File  string `yaml:"file"`
				Tests []int  `yaml:"tests"`
			} `yaml:"source"`
			Config   map[string]any `yaml:"config"`
			Fixtures []any          `yaml:"fixtures"`
			Input    struct {
				Path string `yaml:"path"`
			} `yaml:"input"`
			Steps []struct {
				Config map[string]any `yaml:"config"`
				Input  struct {
					Path string `yaml:"path"`
				} `yaml:"input"`
			} `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	if len(manifest.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(manifest.Sources))
	}
	source := manifest.Sources[0]
	if source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" {
		t.Fatalf("source commit = %q, want pinned Apache APISIX commit", source.Commit)
	}
	if source.File != sourceFile || source.Tests != 17 {
		t.Fatalf("source = (%q, %d tests), want (%q, 17 tests)", source.File, source.Tests, sourceFile)
	}
	if len(manifest.Cases) != 17 {
		t.Fatalf("top-level cases = %d, want exactly 17", len(manifest.Cases))
	}

	names := make(map[string]struct{}, len(manifest.Cases))
	for i, testCase := range manifest.Cases {
		number := i + 1
		if testCase.Name == "" {
			t.Errorf("case %d has no name", number)
		} else if _, exists := names[testCase.Name]; exists {
			t.Errorf("case %d repeats name %q", number, testCase.Name)
		} else {
			names[testCase.Name] = struct{}{}
		}
		if testCase.Source.File != sourceFile {
			t.Errorf("case %d source file = %q, want %q", number, testCase.Source.File, sourceFile)
		}
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != number {
			t.Errorf("case %d source tests = %v, want [%d]", number, testCase.Source.Tests, number)
		}
		if len(testCase.Config) == 0 {
			t.Errorf("case %d %q has no standalone config", number, testCase.Name)
		}
		if !containsSLSLoggerRoute(testCase.Config) {
			t.Errorf("case %d %q has no sls-logger route resource", number, testCase.Name)
		}
		if len(testCase.Fixtures) == 0 {
			t.Errorf("case %d %q has no real fixture", number, testCase.Name)
		}
		if len(testCase.Steps) == 0 {
			t.Errorf("case %d %q has no request steps", number, testCase.Name)
		}
		if testCase.Input.Path == "" && !hasRequestStep(testCase.Steps) {
			t.Errorf("case %d %q has no real request", number, testCase.Name)
		}
	}
}

func containsSLSLoggerRoute(config map[string]any) bool {
	routes, ok := config["routes"].([]any)
	if !ok {
		return false
	}
	for _, rawRoute := range routes {
		route, ok := rawRoute.(map[string]any)
		if !ok {
			continue
		}
		plugins, ok := route["plugins"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := plugins["sls-logger"]; ok {
			return true
		}
	}
	return false
}

func hasRequestStep(steps []struct {
	Config map[string]any `yaml:"config"`
	Input  struct {
		Path string `yaml:"path"`
	} `yaml:"input"`
},
) bool {
	for _, step := range steps {
		if step.Input.Path != "" {
			return true
		}
	}
	return false
}

func assertSLSManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertSLSManifestHasNoAliasesOrMerges(t, child)
	}
}
