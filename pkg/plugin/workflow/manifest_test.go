package workflow

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStandaloneManifestMapsOneIndependentCasePerPinnedBlock(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "workflow.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertWorkflowManifestHasNoAliasesOrMerges(t, &document)

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
			Fixtures []struct {
				Name           string `yaml:"name"`
				Kind           string `yaml:"kind"`
				ExpectRequests *int   `yaml:"expect_requests"`
				Expect         []any  `yaml:"expect"`
				Respond        []any  `yaml:"respond"`
			} `yaml:"fixtures"`
			Steps []struct {
				Name   string         `yaml:"name"`
				Input  map[string]any `yaml:"input"`
				Output map[string]any `yaml:"output"`
			} `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	wantSources := []struct {
		file  string
		tests int
	}{
		{"t/plugin/workflow-without-case.t", 7},
		{"t/plugin/workflow.t", 20},
		{"t/plugin/workflow2.t", 8},
		{"t/plugin/workflow3.t", 7},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	for i, want := range wantSources {
		source := manifest.Sources[i]
		if source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", i+1, source.Commit)
		}
		if source.File != want.file || source.Tests != want.tests {
			t.Fatalf("source %d = (%q, %d), want (%q, %d)", i+1, source.File, source.Tests, want.file, want.tests)
		}
	}
	if len(manifest.Cases) != 42 {
		t.Fatalf("top-level cases = %d, want exactly 42", len(manifest.Cases))
	}

	next := make(map[string]int, len(wantSources))
	for _, source := range wantSources {
		next[source.file] = 1
	}
	seenNames := make(map[string]struct{}, len(manifest.Cases))
	sourceIndex := 0
	sourceCase := 0
	for i, testCase := range manifest.Cases {
		if sourceIndex >= len(wantSources) {
			t.Fatalf("case %d %q exceeds pinned source corpus", i+1, testCase.Name)
		}
		if testCase.Source.File != wantSources[sourceIndex].file {
			t.Errorf(
				"case %d %q source = %q, want block %d from %q",
				i+1,
				testCase.Name,
				testCase.Source.File,
				sourceCase+1,
				wantSources[sourceIndex].file,
			)
		}
		sourceCase++
		if sourceCase == wantSources[sourceIndex].tests {
			sourceIndex++
			sourceCase = 0
		}
		want, ok := next[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != want {
			t.Errorf("case %d %q source tests = %v, want [%d]", i+1, testCase.Name, testCase.Source.Tests, want)
		}
		next[testCase.Source.File]++
		lowerName := strings.ToLower(testCase.Name)
		for _, forbidden := range []string{"placeholder", "generic", "probe", "source-"} {
			if strings.Contains(lowerName, forbidden) {
				t.Errorf("case %d has placeholder-like name %q", i+1, testCase.Name)
			}
		}
		if _, exists := seenNames[testCase.Name]; exists {
			t.Errorf("case %d repeats name %q", i+1, testCase.Name)
		}
		seenNames[testCase.Name] = struct{}{}
		if !containsWorkflowConfig(testCase.Config) {
			t.Errorf("case %d %q has no workflow route/global/consumer config", i+1, testCase.Name)
		}
		if len(testCase.Fixtures) == 0 || len(testCase.Steps) == 0 {
			t.Errorf("case %d %q must use real fixtures and request steps", i+1, testCase.Name)
		}
		for fixtureIndex, fixture := range testCase.Fixtures {
			if fixture.Name == "" || fixture.Kind != "http" || len(fixture.Respond) == 0 {
				t.Errorf(
					"case %d %q fixture %d must be a named executable HTTP fixture",
					i+1,
					testCase.Name,
					fixtureIndex+1,
				)
			}
			if fixture.ExpectRequests == nil && len(fixture.Expect) == 0 {
				t.Errorf(
					"case %d %q fixture %d must assert exact or matched requests",
					i+1,
					testCase.Name,
					fixtureIndex+1,
				)
			}
		}
		for stepIndex, step := range testCase.Steps {
			if step.Name == "" || step.Input["path"] == nil || step.Output["status"] == nil {
				t.Errorf(
					"case %d %q step %d must name a real HTTP request and status assertion",
					i+1,
					testCase.Name,
					stepIndex+1,
				)
			}
		}
	}
	for _, source := range wantSources {
		if got := next[source.file] - 1; got != source.tests {
			t.Fatalf("%s mapped through block %d, want %d", source.file, got, source.tests)
		}
	}
}

func containsWorkflowConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if plugins, ok := typed["plugins"].(map[string]any); ok {
			if _, ok := plugins["workflow"]; ok {
				return true
			}
		}
		for _, child := range typed {
			if containsWorkflowConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsWorkflowConfig)
	}
	return false
}

func assertWorkflowManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertWorkflowManifestHasNoAliasesOrMerges(t, child)
	}
}
