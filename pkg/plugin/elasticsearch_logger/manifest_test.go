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
		Source struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"source"`
		Cases []manifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	if manifest.Source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" {
		t.Fatalf("source commit = %q, want pinned Apache APISIX commit", manifest.Source.Commit)
	}
	if manifest.Source.File != sourceFile || manifest.Source.Tests != 27 {
		t.Fatalf(
			"source = (%q, %d tests), want (%q, 27 tests)",
			manifest.Source.File,
			manifest.Source.Tests,
			sourceFile,
		)
	}
	if len(manifest.Cases) != 27 {
		t.Fatalf("top-level cases = %d, want exactly 27", len(manifest.Cases))
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
		if len(testCase.Fixtures) < 2 {
			t.Errorf("case %d %q has fewer than two real HTTP fixtures", number, testCase.Name)
		}
		if len(testCase.Steps) == 0 {
			t.Errorf("case %d %q has no real request step", number, testCase.Name)
		}
	}
	assertDeterministicMultiEndpointCase(t, manifest.Cases[13])
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

func assertDeterministicMultiEndpointCase(t *testing.T, testCase manifestCase) {
	t.Helper()

	if testCase.Environment["GODEBUG"] != "randautoseed=0" {
		t.Fatalf("source block 14 GODEBUG = %q, want deterministic randautoseed=0", testCase.Environment["GODEBUG"])
	}
	routes := testCase.Config["routes"].([]any)
	route := routes[0].(map[string]any)
	plugins := route["plugins"].(map[string]any)
	logger := plugins["elasticsearch-logger"].(map[string]any)
	endpoints := logger["endpoint_addrs"].([]any)
	if len(endpoints) != 2 ||
		endpoints[0] != "{{FIXTURE.firstSink.URL}}" ||
		endpoints[1] != "{{FIXTURE.secondSink.URL}}" {
		t.Fatalf("source block 14 endpoint_addrs = %v, want two distinct fixture URLs", endpoints)
	}

	fixtures := make(map[string]manifestFixture, len(testCase.Fixtures))
	for _, fixture := range testCase.Fixtures {
		fixtures[fixture.Name] = fixture
	}
	first, firstOK := fixtures["firstSink"]
	second, secondOK := fixtures["secondSink"]
	if !firstOK || !secondOK {
		t.Fatalf("source block 14 fixtures = %v, want firstSink and secondSink", fixtures)
	}
	gets, posts := 0, 0
	for _, fixture := range []manifestFixture{first, second} {
		fixturePosts := 0
		for _, expectation := range fixture.Expect {
			switch expectation.Method {
			case "GET":
				gets++
			case "POST":
				posts++
				fixturePosts++
			}
		}
		if fixturePosts == 0 {
			t.Fatalf("source block 14 fixture %s has no bulk POST expectation", fixture.Name)
		}
	}
	if gets != 1 || posts != 12 {
		t.Fatalf("source block 14 sink expectations = %d GET + %d POST, want 1 GET + 12 POST", gets, posts)
	}
}

func assertNoYAMLAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()

	if node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertNoYAMLAliasesOrMerges(t, child)
	}
}
