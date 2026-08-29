package error_log_logger

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

type errorLogManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Runtime       map[string]any   `yaml:"runtime"`
	Config        map[string]any   `yaml:"config"`
	Fixtures      []map[string]any `yaml:"fixtures"`
	Steps         []map[string]any `yaml:"steps"`
	AfterShutdown []map[string]any `yaml:"after_shutdown"`
}

func TestStandaloneManifestMapsEveryPinnedBlockToIndependentBehavior(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "error-log-logger.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, forbidden := range [][]byte{
		[]byte("source-complete skip"),
		[]byte("standalone fixture is available"),
		[]byte("-source-"),
		[]byte("/probe"),
		[]byte("preserves-upstream"),
	} {
		if bytes.Contains(data, forbidden) {
			t.Errorf("manifest contains generic or placeholder text %q", forbidden)
		}
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertErrorLogManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			Commit      string `yaml:"commit"`
			File        string `yaml:"file"`
			Tests       int    `yaml:"tests"`
			TestNumbers []int  `yaml:"test_numbers"`
		} `yaml:"sources"`
		Cases []errorLogManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	wantSources := []struct {
		file        string
		testNumbers []int
	}{
		{"t/plugin/error-log-logger-clickhouse.t", []int{1, 2, 3, 4, 5, 6, 7, 9}},
		{"t/plugin/error-log-logger-kafka.t", []int{1, 2, 3, 4, 5, 6}},
		{"t/plugin/error-log-logger-skywalking.t", []int{1, 2, 3, 4, 5, 6, 7, 8}},
		{"t/plugin/error-log-logger.t", []int{1, 2, 4, 5, 6, 7, 8, 9, 10, 12, 13, 14, 15}},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	next := make(map[string]int, len(wantSources))
	labels := make(map[string][]int, len(wantSources))
	total := 0
	for i, want := range wantSources {
		source := manifest.Sources[i]
		if source.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", i+1, source.Commit)
		}
		if source.File != want.file || source.Tests != len(want.testNumbers) {
			t.Fatalf(
				"source %d = (%q, %d), want (%q, %d)",
				i+1,
				source.File,
				source.Tests,
				want.file,
				len(want.testNumbers),
			)
		}
		gotNumbers := source.TestNumbers
		if len(gotNumbers) == 0 {
			gotNumbers = make([]int, source.Tests)
			for j := range gotNumbers {
				gotNumbers[j] = j + 1
			}
		}
		if !slices.Equal(gotNumbers, want.testNumbers) {
			t.Fatalf("source %d test_numbers = %v, want %v", i+1, gotNumbers, want.testNumbers)
		}
		labels[want.file] = want.testNumbers
		total += len(want.testNumbers)
	}
	if len(manifest.Cases) != total {
		t.Fatalf("top-level cases = %d, want exactly %d", len(manifest.Cases), total)
	}

	generic := regexp.MustCompile(`(?i)(block-[0-9]+|placeholder|generic|probe|preserves-upstream)`)
	names := make(map[string]struct{}, total)
	for i, testCase := range manifest.Cases {
		index, ok := next[testCase.Source.File]
		if !ok {
			if _, exists := labels[testCase.Source.File]; !exists {
				t.Fatalf("case %d %q has unknown source %q", i+1, testCase.Name, testCase.Source.File)
			}
		}
		if index >= len(labels[testCase.Source.File]) {
			t.Fatalf("case %d %q exceeds selected labels for %q", i+1, testCase.Name, testCase.Source.File)
		}
		want := labels[testCase.Source.File][index]
		if !slices.Equal(testCase.Source.Tests, []int{want}) {
			t.Fatalf("case %d %q source tests = %v, want [%d]", i+1, testCase.Name, testCase.Source.Tests, want)
		}
		next[testCase.Source.File] = index + 1
		if strings.TrimSpace(testCase.Name) == "" || generic.MatchString(testCase.Name) {
			t.Errorf("case %d has generic behavior name %q", i+1, testCase.Name)
		}
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("case %d repeats behavior name %q", i+1, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		assertErrorLogCaseRunsStandalone(t, i+1, testCase)
	}
	for _, source := range wantSources {
		if got := next[source.file]; got != len(source.testNumbers) {
			t.Fatalf("%s mapped through %d blocks, want %d", source.file, got, len(source.testNumbers))
		}
	}
}

func assertErrorLogCaseRunsStandalone(t *testing.T, index int, testCase errorLogManifestCase) {
	t.Helper()
	if len(testCase.Config) == 0 {
		t.Errorf("case %d %q has no standalone resources", index, testCase.Name)
	}
	if !containsErrorLogLoggerConfig(testCase.Config) {
		t.Errorf("case %d %q does not configure error-log-logger metadata or route", index, testCase.Name)
	}
	if len(testCase.Steps) == 0 {
		t.Errorf("case %d %q has no real request/assertion steps", index, testCase.Name)
	}
	plugins, _ := testCase.Runtime["plugins"].([]any)
	if !slices.Contains(plugins, any("error-log-logger")) &&
		!strings.Contains(testCase.Name, "not-enabled") {
		t.Errorf("case %d %q does not enable the global plugin runtime", index, testCase.Name)
	}
	for stepIndex, step := range testCase.Steps {
		input, _ := step["input"].(map[string]any)
		output, _ := step["output"].(map[string]any)
		if strings.TrimSpace(stringValue(step["name"])) == "" ||
			strings.TrimSpace(stringValue(input["path"])) == "" ||
			output["status"] == nil {
			t.Errorf("case %d %q step %d lacks named request/status assertion", index, testCase.Name, stepIndex+1)
		}
	}
}

func containsErrorLogLoggerConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if typed["id"] == "error-log-logger" {
			return true
		}
		for key, child := range typed {
			if key == "error-log-logger" || containsErrorLogLoggerConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsErrorLogLoggerConfig)
	}
	return false
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func assertErrorLogManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML anchor or alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertErrorLogManifestHasNoAliasesOrMerges(t, child)
	}
}
