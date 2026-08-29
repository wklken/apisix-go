package graphql_proxy_cache

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

type graphqlProxyCacheManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Runtime  map[string]any `yaml:"runtime"`
	Config   map[string]any `yaml:"config"`
	Fixtures []struct {
		Kind           string `yaml:"kind"`
		ExpectRequests *int   `yaml:"expect_requests"`
	} `yaml:"fixtures"`
	Steps []struct {
		Name   string         `yaml:"name"`
		Input  map[string]any `yaml:"input"`
		Output map[string]any `yaml:"output"`
	} `yaml:"steps"`
}

func TestStandaloneManifestMapsEveryPinnedBlockToIndependentGraphQLCacheCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "graphql-proxy-cache.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertGraphQLProxyCacheManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			Commit         string `yaml:"commit"`
			File           string `yaml:"file"`
			Tests          int    `yaml:"tests"`
			TestNumbers    []int  `yaml:"test_numbers"`
			RegressionOnly bool   `yaml:"regression_only"`
		} `yaml:"sources"`
		Cases []graphqlProxyCacheManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	wantSources := []struct {
		commit         string
		file           string
		tests          int
		testNumbers    []int
		regressionOnly bool
	}{
		{commit: "9ef2ecab67f652d38365049613610ef649bb4ad0", file: "t/plugin/graphql-proxy-cache/disk.t", tests: 11},
		{
			commit: "9ef2ecab67f652d38365049613610ef649bb4ad0",
			file:   "t/plugin/graphql-proxy-cache/graphql.t", tests: 19,
			testNumbers: []int{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21},
		},
		{commit: "9ef2ecab67f652d38365049613610ef649bb4ad0", file: "t/plugin/graphql-proxy-cache/memory.t", tests: 15},
		{
			commit: "c3d7d5ec69774121f53d2e20d29d09c816795dd7",
			file:   "t/plugin/graphql-proxy-cache/memory.t", tests: 1,
			testNumbers: []int{16}, regressionOnly: true,
		},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	for i, want := range wantSources {
		source := manifest.Sources[i]
		if source.Commit != want.commit || source.File != want.file || source.Tests != want.tests ||
			!slices.Equal(source.TestNumbers, want.testNumbers) || source.RegressionOnly != want.regressionOnly {
			t.Fatalf(
				"source %d = (%q, %q, %d, %v, %v), want (%q, %q, %d, %v, %v)",
				i+1,
				source.Commit,
				source.File,
				source.Tests,
				source.TestNumbers,
				source.RegressionOnly,
				want.commit,
				want.file,
				want.tests,
				want.testNumbers,
				want.regressionOnly,
			)
		}
	}
	if len(manifest.Cases) != 46 {
		t.Fatalf("top-level cases = %d, want exactly 46", len(manifest.Cases))
	}

	genericName := regexp.MustCompile(`(?i)(block-[0-9]+|source-[0-9]+|placeholder|generic|probe|lifecycle)`)
	names := make(map[string]struct{}, len(manifest.Cases))
	wantMappings := map[string][]int{
		"t/plugin/graphql-proxy-cache/disk.t":    {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		"t/plugin/graphql-proxy-cache/graphql.t": {3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21},
		"t/plugin/graphql-proxy-cache/memory.t":  {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	}
	mapped := make(map[string][]int, len(wantMappings))
	for i, testCase := range manifest.Cases {
		if _, ok := wantMappings[testCase.Source.File]; !ok {
			t.Fatalf("case %d %q has unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf("case %d %q source tests = %v, want exactly one", i+1, testCase.Name, testCase.Source.Tests)
		}
		mapped[testCase.Source.File] = append(mapped[testCase.Source.File], testCase.Source.Tests[0])
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("case %d has duplicate behavior name %q", i+1, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		assertGraphQLProxyCacheCaseIdentity(t, i+1, testCase, genericName)
		assertGraphQLProxyCacheCaseResources(t, i+1, testCase)
	}
	for file, want := range wantMappings {
		if !slices.Equal(mapped[file], want) {
			t.Fatalf("%s mappings = %v, want %v", file, mapped[file], want)
		}
	}
}

func assertGraphQLProxyCacheCaseIdentity(
	t *testing.T,
	index int,
	testCase graphqlProxyCacheManifestCase,
	genericName *regexp.Regexp,
) {
	t.Helper()
	source := strings.TrimSuffix(filepath.Base(testCase.Source.File), ".t")
	testNumber := testCase.Source.Tests[0]
	suffix := "-test-" + strconv.Itoa(testNumber)
	if !strings.HasPrefix(testCase.Name, source+"-") || !strings.HasSuffix(testCase.Name, suffix) {
		t.Errorf("case %d %q does not encode source %q and test %d", index, testCase.Name, source, testNumber)
	}
	behavior := strings.TrimSuffix(strings.TrimPrefix(testCase.Name, source+"-"), suffix)
	if strings.TrimSpace(behavior) == "" || genericName.MatchString(testCase.Name) {
		t.Errorf("case %d has generic behavior name %q", index, testCase.Name)
	}
	if len(testCase.Steps) == 0 {
		t.Errorf("case %d %q has no real request step", index, testCase.Name)
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

func assertGraphQLProxyCacheCaseResources(t *testing.T, index int, testCase graphqlProxyCacheManifestCase) {
	t.Helper()
	if !containsGraphQLProxyCacheConfig(testCase.Config) {
		t.Errorf("case %d %q has no route that configures graphql-proxy-cache", index, testCase.Name)
	}
	if len(testCase.Fixtures) == 0 {
		t.Errorf("case %d %q has no real upstream fixture", index, testCase.Name)
	}
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
		if fixture.ExpectRequests == nil {
			t.Errorf(
				"case %d %q fixture %d lacks exact upstream request-count assertion",
				index,
				testCase.Name,
				fixtureIndex+1,
			)
		}
	}
	apisix, ok := testCase.Runtime["apisix"].(map[string]any)
	if !ok {
		t.Errorf("case %d %q lacks apisix runtime configuration", index, testCase.Name)
		return
	}
	if _, ok := apisix["proxy_cache"].(map[string]any); !ok {
		t.Errorf("case %d %q lacks apisix.proxy_cache zone runtime configuration", index, testCase.Name)
	}
}

func containsGraphQLProxyCacheConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if plugins, ok := typed["plugins"].(map[string]any); ok {
			if _, ok := plugins["graphql-proxy-cache"]; ok {
				return true
			}
		}
		for _, child := range typed {
			if containsGraphQLProxyCacheConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsGraphQLProxyCacheConfig)
	}
	return false
}

func assertGraphQLProxyCacheManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML anchor or alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertGraphQLProxyCacheManifestHasNoAliasesOrMerges(t, child)
	}
}
