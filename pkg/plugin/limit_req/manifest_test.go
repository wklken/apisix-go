package limit_req

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type limitReqManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Config map[string]any `yaml:"config"`
	Input  map[string]any `yaml:"input"`
	Output map[string]any `yaml:"output"`
	Steps  []struct {
		Input  map[string]any `yaml:"input"`
		Output map[string]any `yaml:"output"`
	} `yaml:"steps"`
}

func TestStandaloneManifestMapsEveryLocalSourceBlockToIndependentCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "limit-req.yaml")
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
		Cases []limitReqManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	const (
		sourceFile   = "t/plugin/limit-req.t"
		sourceCommit = "c3d7d5ec69774121f53d2e20d29d09c816795dd7"
		sourceTests  = 21
	)
	assertLocalSourceCasesDoNotUseYAMLAliases(t, data, sourceFile)
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

	targetCases := make([]limitReqManifestCase, 0, sourceTests)
	for _, testCase := range manifest.Cases {
		if testCase.Source.File == sourceFile {
			targetCases = append(targetCases, testCase)
		}
	}
	if len(targetCases) != sourceTests {
		t.Fatalf("%s cases = %d, want exactly %d", sourceFile, len(targetCases), sourceTests)
	}

	genericName := regexp.MustCompile(`(?i)(placeholder|generic|probe|block-[0-9]+|source-[0-9]+)`)
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
			t.Errorf("case %d has generic name %q", testNumber, testCase.Name)
		}
		if _, exists := names[testCase.Name]; exists {
			t.Errorf("case %d duplicates behavior name %q", testNumber, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if !containsLimitReqConfig(testCase.Config) {
			t.Errorf("case %d %q has no real limit-req resource config", testNumber, testCase.Name)
		}
		if testNumber == 2 {
			if !strings.Contains(testCase.Name, "misfiled-limit-conn") {
				t.Errorf("case 2 name %q does not classify the pinned misfiled limit-conn test", testCase.Name)
			}
			if !containsPluginConfig(testCase.Config, "limit-conn") {
				t.Error("case 2 does not preserve the pinned misfiled limit-conn configuration")
			}
		}
		if len(testCase.Steps) == 0 {
			if len(testCase.Input) == 0 || len(testCase.Output) == 0 {
				t.Errorf("case %d %q has no executable request/response assertion", testNumber, testCase.Name)
			}
			continue
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

func TestStandaloneManifestMapsEveryRedisSourceBlockToIndependentCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "limit-req.yaml")
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
		Cases []limitReqManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	const (
		sourceFile   = "t/plugin/limit-req-redis.t"
		sourceCommit = "c3d7d5ec69774121f53d2e20d29d09c816795dd7"
		sourceTests  = 30
	)
	assertLocalSourceCasesDoNotUseYAMLAliases(t, data, sourceFile)
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

	targetCases := make([]limitReqManifestCase, 0, sourceTests)
	for _, testCase := range manifest.Cases {
		if testCase.Source.File == sourceFile {
			targetCases = append(targetCases, testCase)
		}
	}
	if len(targetCases) != sourceTests {
		t.Fatalf("%s cases = %d, want exactly %d", sourceFile, len(targetCases), sourceTests)
	}

	genericName := regexp.MustCompile(`(?i)(placeholder|generic|probe|block-[0-9]+|source-[0-9]+)`)
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
			t.Errorf("case %d has generic name %q", testNumber, testCase.Name)
		}
		if _, exists := names[testCase.Name]; exists {
			t.Errorf("case %d duplicates behavior name %q", testNumber, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if !containsLimitReqConfig(testCase.Config) {
			t.Errorf("case %d %q has no real limit-req resource config", testNumber, testCase.Name)
		}
		if testNumber != 24 && !containsConfigValue(testCase.Config, "policy", "redis") {
			t.Errorf("case %d %q does not exercise the pinned Redis policy", testNumber, testCase.Name)
		}
		if len(testCase.Steps) == 0 {
			if len(testCase.Input) == 0 || len(testCase.Output) == 0 {
				t.Errorf("case %d %q has no executable request/response assertion", testNumber, testCase.Name)
			}
			continue
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

func TestStandaloneManifestMapsEveryRedisClusterSourceBlockToIndependentCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "limit-req.yaml")
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
		Cases []limitReqManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	const (
		sourceFile   = "t/plugin/limit-req-redis-cluster.t"
		sourceCommit = "c3d7d5ec69774121f53d2e20d29d09c816795dd7"
		sourceTests  = 22
	)
	assertLocalSourceCasesDoNotUseYAMLAliases(t, data, sourceFile)
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

	targetCases := make([]limitReqManifestCase, 0, sourceTests)
	for _, testCase := range manifest.Cases {
		if testCase.Source.File == sourceFile {
			targetCases = append(targetCases, testCase)
		}
	}
	if len(targetCases) != sourceTests {
		t.Fatalf("%s cases = %d, want exactly %d", sourceFile, len(targetCases), sourceTests)
	}

	genericName := regexp.MustCompile(`(?i)(placeholder|generic|probe|block-[0-9]+|source-[0-9]+)`)
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
			t.Errorf("case %d has generic name %q", testNumber, testCase.Name)
		}
		if _, exists := names[testCase.Name]; exists {
			t.Errorf("case %d duplicates behavior name %q", testNumber, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if !containsLimitReqConfig(testCase.Config) {
			t.Errorf("case %d %q has no real limit-req resource config", testNumber, testCase.Name)
		}
		if testNumber != 21 && !containsConfigValue(testCase.Config, "policy", "redis-cluster") {
			t.Errorf("case %d %q does not exercise the pinned redis-cluster policy", testNumber, testCase.Name)
		}
		if testNumber == 2 {
			if !containsConfigValue(testCase.Config, "redis_cluster_ssl", true) {
				t.Error("case 2 does not preserve the pinned redis-cluster SSL configuration")
			}
		}
		if len(testCase.Steps) == 0 {
			if len(testCase.Input) == 0 || len(testCase.Output) == 0 {
				t.Errorf("case %d %q has no executable request/response assertion", testNumber, testCase.Name)
			}
			continue
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

func TestStandaloneManifestMapsEveryRemainingSourceBlockToIndependentCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "limit-req.yaml")
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
		Cases []limitReqManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	const sourceCommit = "c3d7d5ec69774121f53d2e20d29d09c816795dd7"
	remainingSources := []string{
		"t/plugin/limit-req-shared-counter.t",
		"t/plugin/limit-req2.t",
		"t/plugin/limit-req3.t",
	}
	for _, sourceFile := range remainingSources {
		t.Run(sourceFile, func(t *testing.T) {
			sourceTests := 0
			sourceFound := false
			for _, source := range manifest.Sources {
				if source.File != sourceFile {
					continue
				}
				sourceFound = true
				sourceTests = source.Tests
				if source.Commit != sourceCommit || source.Tests <= 0 {
					t.Fatalf(
						"%s source = commit %q tests %d, want commit %q with tests",
						sourceFile,
						source.Commit,
						source.Tests,
						sourceCommit,
					)
				}
			}
			if !sourceFound {
				t.Fatalf("source %s is not declared", sourceFile)
			}

			assertLocalSourceCasesDoNotUseYAMLAliases(t, data, sourceFile)
			targetCases := make([]limitReqManifestCase, 0, sourceTests)
			for _, testCase := range manifest.Cases {
				if testCase.Source.File == sourceFile {
					targetCases = append(targetCases, testCase)
				}
			}
			if len(targetCases) != sourceTests {
				t.Fatalf("%s cases = %d, want exactly %d", sourceFile, len(targetCases), sourceTests)
			}

			genericName := regexp.MustCompile(`(?i)(placeholder|generic|probe|block-[0-9]+|source-[0-9]+)`)
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
				if genericName.MatchString(testCase.Name) {
					t.Errorf("case %d has generic name %q", testNumber, testCase.Name)
				}
				if _, exists := names[testCase.Name]; exists {
					t.Errorf("case %d duplicates behavior name %q", testNumber, testCase.Name)
				}
				names[testCase.Name] = struct{}{}
				if !containsLimitReqConfig(testCase.Config) {
					t.Errorf("case %d %q has no real limit-req resource config", testNumber, testCase.Name)
				}
				if len(testCase.Steps) == 0 {
					if len(testCase.Input) == 0 || len(testCase.Output) == 0 {
						t.Errorf("case %d %q has no executable request/response assertion", testNumber, testCase.Name)
					}
					continue
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
		})
	}
}

func assertLocalSourceCasesDoNotUseYAMLAliases(t *testing.T, data []byte, sourceFile string) {
	t.Helper()

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode manifest syntax tree: %v", err)
	}
	if len(document.Content) != 1 {
		t.Fatalf("manifest syntax tree has %d roots, want 1", len(document.Content))
	}
	cases := yamlMappingValue(document.Content[0], "cases")
	if cases == nil || cases.Kind != yaml.SequenceNode {
		t.Fatal("manifest cases is not a sequence")
	}
	for _, testCase := range cases.Content {
		source := yamlMappingValue(testCase, "source")
		file := yamlMappingValue(source, "file")
		if file == nil || file.Value != sourceFile {
			continue
		}
		if yamlNodeUsesAliasOrMerge(testCase) {
			name := yamlMappingValue(testCase, "name")
			t.Errorf("case %q uses a YAML alias or merge instead of independent config", name.Value)
		}
	}
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
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

func yamlNodeUsesAliasOrMerge(node *yaml.Node) bool {
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
	return slices.ContainsFunc(node.Content, yamlNodeUsesAliasOrMerge)
}

func containsLimitReqConfig(value any) bool {
	return containsPluginConfig(value, name)
}

func containsPluginConfig(value any, pluginName string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == pluginName {
				if _, ok := child.(map[string]any); ok {
					return true
				}
			}
			if containsPluginConfig(child, pluginName) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, func(child any) bool {
			return containsPluginConfig(child, pluginName)
		})
	case map[any]any:
		for key, child := range typed {
			if fmt.Sprint(key) == pluginName {
				if _, ok := child.(map[string]any); ok {
					return true
				}
			}
			if containsPluginConfig(child, pluginName) {
				return true
			}
		}
	}
	return false
}

func containsConfigValue(value any, key string, expected any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for currentKey, child := range typed {
			if currentKey == key && child == expected {
				return true
			}
			if containsConfigValue(child, key, expected) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, func(child any) bool {
			return containsConfigValue(child, key, expected)
		})
	case map[any]any:
		for currentKey, child := range typed {
			if fmt.Sprint(currentKey) == key && child == expected {
				return true
			}
			if containsConfigValue(child, key, expected) {
				return true
			}
		}
	}
	return false
}
