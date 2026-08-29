package proxy_cache

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

type proxyCacheManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Runtime  map[string]any `yaml:"runtime"`
	Config   map[string]any `yaml:"config"`
	Fixtures []struct {
		Kind           string         `yaml:"kind"`
		ExpectRequests *int           `yaml:"expect_requests"`
		Count          map[string]any `yaml:"count"`
	} `yaml:"fixtures"`
	Steps []proxyCacheManifestStep `yaml:"steps"`
}

type proxyCacheManifestStep struct {
	Name           string           `yaml:"name"`
	Input          map[string]any   `yaml:"input"`
	Output         map[string]any   `yaml:"output"`
	FileAssertions []map[string]any `yaml:"file_assertions"`
}

func TestStandaloneManifestMapsEveryPinnedBlockToIndependentCacheCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "proxy-cache.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertProxyCacheManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"sources"`
		Cases []proxyCacheManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	wantSources := []struct {
		file  string
		tests int
	}{
		{"t/plugin/proxy-cache/disk.t", 29},
		{"t/plugin/proxy-cache/memory.t", 47},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	total := 0
	next := make(map[string]int, len(wantSources))
	for i, want := range wantSources {
		source := manifest.Sources[i]
		if source.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", i+1, source.Commit)
		}
		if source.File != want.file || source.Tests != want.tests {
			t.Fatalf(
				"source %d = (%q, %d), want (%q, %d)",
				i+1,
				source.File,
				source.Tests,
				want.file,
				want.tests,
			)
		}
		total += want.tests
		next[want.file] = 1
	}
	if len(manifest.Cases) != total {
		t.Fatalf("top-level cases = %d, want exactly %d", len(manifest.Cases), total)
	}

	genericName := regexp.MustCompile(`(?i)(block-[0-9]+|source-[0-9]+|placeholder|generic|probe|lifecycle)`)
	names := make(map[string]struct{}, total)
	for i, testCase := range manifest.Cases {
		want, ok := next[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != want {
			t.Fatalf(
				"case %d %q source tests = %v, want [%d]",
				i+1,
				testCase.Name,
				testCase.Source.Tests,
				want,
			)
		}
		next[testCase.Source.File]++
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("case %d has duplicate behavior name %q", i+1, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if genericName.MatchString(testCase.Name) {
			t.Errorf("case %d has generic behavior name %q", i+1, testCase.Name)
		}
		assertProxyCacheCaseIdentity(t, i+1, testCase, genericName)
		assertProxyCacheCaseResources(t, i+1, testCase)
	}
	for _, source := range wantSources {
		if got := next[source.file] - 1; got != source.tests {
			t.Fatalf("%s mapped through block %d, want %d", source.file, got, source.tests)
		}
	}
}

func assertProxyCacheCaseIdentity(
	t *testing.T,
	index int,
	testCase proxyCacheManifestCase,
	genericName *regexp.Regexp,
) {
	t.Helper()
	source := strings.TrimSuffix(filepath.Base(testCase.Source.File), ".t")
	testNumber := testCase.Source.Tests[0]
	suffix := "-test-" + strconv.Itoa(testNumber)
	if !strings.HasPrefix(testCase.Name, source+"-") || !strings.HasSuffix(testCase.Name, suffix) {
		t.Errorf(
			"case %d %q does not encode source %q and test %d",
			index,
			testCase.Name,
			source,
			testNumber,
		)
	}
	behavior := strings.TrimSuffix(strings.TrimPrefix(testCase.Name, source+"-"), suffix)
	if strings.TrimSpace(behavior) == "" {
		t.Errorf("case %d %q has no behavior identity", index, testCase.Name)
	}
	for stepIndex, step := range testCase.Steps {
		if strings.TrimSpace(step.Name) == "" || genericName.MatchString(step.Name) {
			t.Errorf("case %d %q step %d has generic behavior name %q", index, testCase.Name, stepIndex+1, step.Name)
		}
		if len(step.Input) == 0 || len(step.Output) == 0 {
			t.Errorf("case %d %q step %d lacks request or response assertions", index, testCase.Name, stepIndex+1)
		}
	}
}

func assertProxyCacheCaseResources(t *testing.T, index int, testCase proxyCacheManifestCase) {
	t.Helper()
	if !containsProxyCacheConfig(testCase.Config) {
		t.Errorf("case %d %q has no route that configures proxy-cache", index, testCase.Name)
	}
	if len(testCase.Fixtures) == 0 {
		t.Errorf("case %d %q has no real upstream fixture", index, testCase.Name)
	} else {
		for fixtureIndex, fixture := range testCase.Fixtures {
			if fixture.Kind != "http" {
				t.Errorf(
					"case %d %q fixture %d kind = %q, want real HTTP origin",
					index,
					testCase.Name,
					fixtureIndex+1,
					fixture.Kind,
				)
			}
			if fixture.ExpectRequests == nil && len(fixture.Count) == 0 {
				t.Errorf(
					"case %d %q fixture %d lacks exact upstream request-count assertion",
					index,
					testCase.Name,
					fixtureIndex+1,
				)
			}
		}
	}
	if len(testCase.Steps) == 0 {
		t.Errorf("case %d %q has no real request step", index, testCase.Name)
	}
	apisix, ok := testCase.Runtime["apisix"].(map[string]any)
	if !ok {
		t.Errorf("case %d %q lacks apisix runtime configuration", index, testCase.Name)
		return
	}
	if _, ok := apisix["proxy_cache"].(map[string]any); !ok {
		t.Errorf("case %d %q lacks apisix.proxy_cache zone runtime configuration", index, testCase.Name)
	}
	if testCase.Source.File == "t/plugin/proxy-cache/disk.t" &&
		slices.Contains([]int{1, 8, 9, 11, 12}, testCase.Source.Tests[0]) &&
		!slices.ContainsFunc(testCase.Steps, func(step proxyCacheManifestStep) bool {
			return len(step.FileAssertions) > 0
		}) {
		t.Errorf("case %d %q lacks required disk-file lifecycle assertion", index, testCase.Name)
	}
}

func containsProxyCacheConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if plugins, ok := typed["plugins"].(map[string]any); ok {
			if _, ok := plugins["proxy-cache"]; ok {
				return true
			}
		}
		for _, child := range typed {
			if containsProxyCacheConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsProxyCacheConfig)
	}
	return false
}

func assertProxyCacheManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML anchor or alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertProxyCacheManifestHasNoAliasesOrMerges(t, child)
	}
}
