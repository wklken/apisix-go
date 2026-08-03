package kafka_logger

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type kafkaLoggerManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Config   map[string]any   `yaml:"config"`
	Fixtures []map[string]any `yaml:"fixtures"`
	Steps    []map[string]any `yaml:"steps"`
	Variants []struct {
		Name   string           `yaml:"name"`
		Config map[string]any   `yaml:"config"`
		Steps  []map[string]any `yaml:"steps"`
	} `yaml:"variants"`
}

func TestStandaloneManifestMapsEveryPinnedBlockToIndependentBehavior(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "kafka-logger.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, forbidden := range [][]byte{
		[]byte("source-complete skip"),
		[]byte("standalone fixture is available"),
		[]byte("kafka-logger-source-"),
		[]byte("/probe"),
		[]byte("preserves-upstream"),
	} {
		if bytes.Contains(data, forbidden) {
			t.Errorf("manifest contains grouped or placeholder text %q", forbidden)
		}
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertKafkaLoggerManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"sources"`
		Cases []kafkaLoggerManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	wantSources := []struct {
		file  string
		tests int
	}{
		{"t/plugin/kafka-logger-large-body.t", 28},
		{"t/plugin/kafka-logger-log-format.t", 5},
		{"t/plugin/kafka-logger.t", 29},
		{"t/plugin/kafka-logger2.t", 27},
		{"t/plugin/kafka-logger3.t", 2},
		{"t/plugin/kafka-logger4.t", 8},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	next := make(map[string]int, len(wantSources))
	total := 0
	for index, want := range wantSources {
		source := manifest.Sources[index]
		if source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", index+1, source.Commit)
		}
		if source.File != want.file || source.Tests != want.tests {
			t.Fatalf("source %d = (%q, %d), want (%q, %d)", index+1, source.File, source.Tests, want.file, want.tests)
		}
		next[want.file] = 1
		total += want.tests
	}
	if len(manifest.Cases) != total {
		t.Fatalf("top-level cases = %d, want exactly %d", len(manifest.Cases), total)
	}

	generic := regexp.MustCompile(`(?i)(block-[0-9]+|placeholder|generic|probe|preserves-upstream|source-[0-9]+)`)
	names := make(map[string]struct{}, total)
	for index, testCase := range manifest.Cases {
		want, ok := next[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unknown source %q", index+1, testCase.Name, testCase.Source.File)
		}
		if !slices.Equal(testCase.Source.Tests, []int{want}) {
			t.Fatalf("case %d %q source tests = %v, want [%d]", index+1, testCase.Name, testCase.Source.Tests, want)
		}
		next[testCase.Source.File]++
		if strings.TrimSpace(testCase.Name) == "" || generic.MatchString(testCase.Name) {
			t.Errorf("case %d has generic behavior name %q", index+1, testCase.Name)
		}
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("case %d repeats behavior name %q", index+1, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		assertKafkaLoggerCaseRunsStandalone(t, index+1, testCase)
	}
	for _, source := range wantSources {
		if got := next[source.file] - 1; got != source.tests {
			t.Fatalf("%s mapped through block %d, want %d", source.file, got, source.tests)
		}
	}
}

func assertKafkaLoggerCaseRunsStandalone(t *testing.T, index int, testCase kafkaLoggerManifestCase) {
	t.Helper()
	if len(testCase.Variants) == 0 {
		assertKafkaLoggerScenarioRunsStandalone(t, index, testCase.Name, testCase.Config, testCase.Steps)
		return
	}
	for variantIndex, variant := range testCase.Variants {
		name := testCase.Name + "/" + variant.Name
		if strings.TrimSpace(variant.Name) == "" {
			t.Errorf("case %d variant %d has no behavior name", index, variantIndex+1)
		}
		assertKafkaLoggerScenarioRunsStandalone(t, index, name, variant.Config, variant.Steps)
	}
}

func assertKafkaLoggerScenarioRunsStandalone(
	t *testing.T,
	index int,
	name string,
	config map[string]any,
	steps []map[string]any,
) {
	t.Helper()
	if len(config) == 0 {
		t.Errorf("case %d %q has no standalone resources", index, name)
	}
	if !containsKafkaLoggerConfig(config) {
		t.Errorf("case %d %q does not configure kafka-logger", index, name)
	}
	if len(steps) == 0 {
		t.Errorf("case %d %q has no real request/assertion steps", index, name)
	}
	for stepIndex, step := range steps {
		input, _ := step["input"].(map[string]any)
		output, _ := step["output"].(map[string]any)
		if strings.TrimSpace(kafkaLoggerStringValue(step["name"])) == "" ||
			strings.TrimSpace(kafkaLoggerStringValue(input["path"])) == "" ||
			output["status"] == nil {
			t.Errorf("case %d %q step %d lacks named request/status assertion", index, name, stepIndex+1)
		}
	}
}

func containsKafkaLoggerConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if _, ok := typed["kafka-logger"]; ok {
			return true
		}
		for _, child := range typed {
			if containsKafkaLoggerConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsKafkaLoggerConfig)
	}
	return false
}

func kafkaLoggerStringValue(value any) string {
	result, _ := value.(string)
	return result
}

func assertKafkaLoggerManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML anchor or alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertKafkaLoggerManifestHasNoAliasesOrMerges(t, child)
	}
}
