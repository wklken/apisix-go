package graphql_limit_count

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type graphqlLimitCountManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Config map[string]any `yaml:"config"`
	Steps  []struct {
		Input  map[string]any `yaml:"input"`
		Output map[string]any `yaml:"output"`
	} `yaml:"steps"`
}

func TestStandaloneManifestMapsEveryPinnedSourceBlockToIndependentCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "graphql-limit-count.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest struct {
		Sources []struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"sources"`
		Cases []graphqlLimitCountManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	const (
		sourceFile   = "t/plugin/graphql-limit-count.t"
		sourceCommit = "c3d7d5ec69774121f53d2e20d29d09c816795dd7"
		sourceTests  = 26
	)
	sourceFound := false
	for _, source := range manifest.Sources {
		if source.File != sourceFile {
			continue
		}
		sourceFound = true
		if source.Commit != sourceCommit || source.Tests != sourceTests {
			t.Fatalf(
				"%s source = commit %q tests %d, want commit %q tests %d",
				sourceFile,
				source.Commit,
				source.Tests,
				sourceCommit,
				sourceTests,
			)
		}
	}
	if !sourceFound {
		t.Fatalf("source %s is not declared", sourceFile)
	}

	assertGraphQLSourceCasesDoNotUseYAMLAliases(t, data, sourceFile)
	targetCases := make([]graphqlLimitCountManifestCase, 0, sourceTests)
	for _, testCase := range manifest.Cases {
		if testCase.Source.File == sourceFile {
			targetCases = append(targetCases, testCase)
		}
	}
	if len(targetCases) != sourceTests {
		t.Fatalf("%s cases = %d, want exactly %d", sourceFile, len(targetCases), sourceTests)
	}

	genericName := regexp.MustCompile(`(?i)(placeholder|generic|probe|skip|block-[0-9]+|source-[0-9]+)`)
	names := make(map[string]struct{}, sourceTests)
	for i, testCase := range targetCases {
		testNumber := i + 1
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != testNumber {
			t.Fatalf(
				"case %d %q source tests = %v, want [%d]",
				testNumber,
				testCase.Name,
				testCase.Source.Tests,
				testNumber,
			)
		}
		if !strings.HasSuffix(testCase.Name, "-test-"+strconv.Itoa(testNumber)) {
			t.Errorf("case %d name %q does not end in its source identity", testNumber, testCase.Name)
		}
		if genericName.MatchString(testCase.Name) {
			t.Errorf("case %d has non-behavioral name %q", testNumber, testCase.Name)
		}
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("case %d duplicates behavior name %q", testNumber, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		encodedConfig, err := yaml.Marshal(testCase.Config)
		if err != nil {
			t.Fatalf("case %d marshal config: %v", testNumber, err)
		}
		if !strings.Contains(string(encodedConfig), "graphql-limit-count:") {
			t.Errorf("case %d %q has no real graphql-limit-count resource config", testNumber, testCase.Name)
		}
		if len(testCase.Steps) == 0 {
			t.Errorf("case %d %q has no executable request/response assertion", testNumber, testCase.Name)
		}
		for stepIndex, step := range testCase.Steps {
			if len(step.Input) == 0 || len(step.Output) == 0 {
				t.Errorf(
					"case %d %q step %d has no request/response assertion",
					testNumber,
					testCase.Name,
					stepIndex+1,
				)
			}
		}
	}
}

func assertGraphQLSourceCasesDoNotUseYAMLAliases(t *testing.T, data []byte, sourceFile string) {
	t.Helper()

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode manifest syntax tree: %v", err)
	}
	if len(document.Content) != 1 {
		t.Fatalf("manifest syntax tree has %d roots, want 1", len(document.Content))
	}
	cases := graphqlYAMLMappingValue(document.Content[0], "cases")
	if cases == nil || cases.Kind != yaml.SequenceNode {
		t.Fatal("manifest cases is not a sequence")
	}
	for _, testCase := range cases.Content {
		source := graphqlYAMLMappingValue(testCase, "source")
		file := graphqlYAMLMappingValue(source, "file")
		if file == nil || file.Value != sourceFile {
			continue
		}
		if graphqlYAMLNodeUsesAliasOrMerge(testCase) {
			name := graphqlYAMLMappingValue(testCase, "name")
			t.Errorf("case %q uses a YAML alias or merge instead of independent config", name.Value)
		}
	}
}

func graphqlYAMLMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func graphqlYAMLNodeUsesAliasOrMerge(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.AliasNode {
		return true
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == "<<" {
				return true
			}
		}
	}
	return slices.ContainsFunc(node.Content, graphqlYAMLNodeUsesAliasOrMerge)
}
