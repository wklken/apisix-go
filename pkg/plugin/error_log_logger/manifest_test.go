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
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"sources"`
		Cases []errorLogManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	wantSources := []struct {
		file  string
		tests int
	}{
		{"t/plugin/error-log-logger-clickhouse.t", 9},
		{"t/plugin/error-log-logger-kafka.t", 7},
		{"t/plugin/error-log-logger-skywalking.t", 8},
		{"t/plugin/error-log-logger.t", 15},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	next := make(map[string]int, len(wantSources))
	total := 0
	for i, want := range wantSources {
		source := manifest.Sources[i]
		if source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", i+1, source.Commit)
		}
		if source.File != want.file || source.Tests != want.tests {
			t.Fatalf("source %d = (%q, %d), want (%q, %d)", i+1, source.File, source.Tests, want.file, want.tests)
		}
		next[want.file] = 1
		total += want.tests
	}
	if len(manifest.Cases) != total {
		t.Fatalf("top-level cases = %d, want exactly %d", len(manifest.Cases), total)
	}

	generic := regexp.MustCompile(`(?i)(block-[0-9]+|placeholder|generic|probe|preserves-upstream)`)
	names := make(map[string]struct{}, total)
	for i, testCase := range manifest.Cases {
		want, ok := next[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		if !slices.Equal(testCase.Source.Tests, []int{want}) {
			t.Fatalf("case %d %q source tests = %v, want [%d]", i+1, testCase.Name, testCase.Source.Tests, want)
		}
		next[testCase.Source.File]++
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
		if got := next[source.file] - 1; got != source.tests {
			t.Fatalf("%s mapped through block %d, want %d", source.file, got, source.tests)
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
