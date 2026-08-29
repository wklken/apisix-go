package elasticsearch_logger

import (
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStandaloneManifestMapsExactUpstreamBlocks(t *testing.T) {
	const sourceFile = "t/plugin/elasticsearch-logger.t"

	path := filepath.Join("..", "..", "..", "t", "plugin", "elasticsearch-logger.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertNoYAMLAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			Commit      string `yaml:"commit"`
			File        string `yaml:"file"`
			Tests       int    `yaml:"tests"`
			TestNumbers []int  `yaml:"test_numbers"`
		} `yaml:"sources"`
		Cases []manifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	if len(manifest.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(manifest.Sources))
	}
	primary := manifest.Sources[0]
	if primary.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("primary source commit = %q, want pinned Apache APISIX commit", primary.Commit)
	}
	if primary.File != sourceFile || primary.Tests != 24 {
		t.Fatalf(
			"primary source = (%q, %d tests), want (%q, 24 selected tests)",
			primary.File,
			primary.Tests,
			sourceFile,
		)
	}
	wantPrimaryNumbers := []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26}
	if len(primary.TestNumbers) != len(wantPrimaryNumbers) {
		t.Fatalf("primary test_numbers = %v, want %v", primary.TestNumbers, wantPrimaryNumbers)
	}
	for i, want := range wantPrimaryNumbers {
		if primary.TestNumbers[i] != want {
			t.Fatalf("primary test_numbers = %v, want %v", primary.TestNumbers, wantPrimaryNumbers)
		}
	}
	secondary := manifest.Sources[1]
	if secondary.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" ||
		secondary.File != "t/plugin/elasticsearch-logger2.t" || secondary.Tests != 8 {
		t.Fatalf(
			"secondary source = (%q, %d tests), want elasticsearch-logger2.t with 8 selected tests",
			secondary.File,
			secondary.Tests,
		)
	}
	for i, want := range []int{1, 2, 3, 5, 6, 7, 8, 9} {
		if secondary.TestNumbers[i] != want {
			t.Fatalf("secondary test_numbers = %v, want [1 2 3 5 6 7 8 9]", secondary.TestNumbers)
		}
	}

	primaryCases := 0
	for _, testCase := range manifest.Cases {
		if testCase.Source.File != sourceFile {
			continue
		}
		primaryCases++
		if primaryCases > len(wantPrimaryNumbers) {
			t.Fatalf("primary manifest has more than %d selected cases", len(wantPrimaryNumbers))
		}
		wantNumber := wantPrimaryNumbers[primaryCases-1]
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != wantNumber {
			t.Errorf("primary case %d source tests = %v, want [%d]", primaryCases, testCase.Source.Tests, wantNumber)
		}
		if len(testCase.Config) == 0 {
			t.Errorf("primary case %d %q has no standalone config", primaryCases, testCase.Name)
		}
		if len(testCase.Fixtures) < 2 {
			t.Errorf("primary case %d %q has fewer than two real HTTP fixtures", primaryCases, testCase.Name)
		}
		if len(testCase.Steps) == 0 {
			t.Errorf("primary case %d %q has no real request step", primaryCases, testCase.Name)
		}
	}
	if primaryCases != len(wantPrimaryNumbers) {
		t.Fatalf("primary cases = %d, want exactly %d", primaryCases, len(wantPrimaryNumbers))
	}
	assertSecondarySourceCases(t, manifest.Cases)
}

type manifestSource struct {
	File  string `yaml:"file"`
	Tests []int  `yaml:"tests"`
}

type manifestCase struct {
	Name        string            `yaml:"name"`
	Source      manifestSource    `yaml:"source"`
	Environment map[string]string `yaml:"environment"`
	Config      map[string]any    `yaml:"config"`
	Fixtures    []manifestFixture `yaml:"fixtures"`
	Steps       []any             `yaml:"steps"`
}

type manifestFixture struct {
	Name   string `yaml:"name"`
	Expect []struct {
		Method string `yaml:"method"`
	} `yaml:"expect"`
}

func assertSecondarySourceCases(t *testing.T, cases []manifestCase) {
	t.Helper()
	const secondaryFile = "t/plugin/elasticsearch-logger2.t"
	secondary := make([]manifestCase, 0, 8)
	labels := 0
	for _, testCase := range cases {
		if testCase.Source.File == secondaryFile {
			secondary = append(secondary, testCase)
			labels += len(testCase.Source.Tests)
		}
	}
	if len(secondary) != 7 {
		t.Fatalf("elasticsearch-logger2.t cases = %d, want 7", len(secondary))
	}
	if labels != 8 {
		t.Fatalf("elasticsearch-logger2.t mapped labels = %d, want 8", labels)
	}
	for _, testCase := range secondary {
		if len(testCase.Config) == 0 {
			t.Errorf("case %q has no standalone config", testCase.Name)
		}
		if len(testCase.Fixtures) < 2 {
			t.Errorf("case %q has fewer than two real HTTP fixtures", testCase.Name)
		}
		if len(testCase.Steps) == 0 {
			t.Errorf("case %q has no real request step", testCase.Name)
		}
	}
}

func assertNoYAMLAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML anchor or alias %q", node.Value)
	}
	for _, child := range node.Content {
		assertNoYAMLAliasesOrMerges(t, child)
	}
}
