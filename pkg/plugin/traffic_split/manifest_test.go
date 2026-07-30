package traffic_split

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStandaloneManifestMapsEveryPinnedBlockToOneBehavioralCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "traffic-split.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read traffic-split manifest: %v", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode traffic-split YAML syntax tree: %v", err)
	}
	assertTrafficSplitManifestHasNoAliasesOrMerges(t, &document)

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
				NetworkExpect  []any  `yaml:"network_expect"`
				NetworkRespond []any  `yaml:"network_respond"`
			} `yaml:"fixtures"`
			Steps []struct {
				Name          string         `yaml:"name"`
				Config        map[string]any `yaml:"config"`
				ConfigProbe   map[string]any `yaml:"config_probe"`
				ConfigTimeout string         `yaml:"config_timeout"`
				Input         map[string]any `yaml:"input"`
				Output        map[string]any `yaml:"output"`
			} `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode traffic-split manifest: %v", err)
	}

	wantSources := []struct {
		file  string
		tests int
	}{
		{"t/plugin/traffic-split.t", 21},
		{"t/plugin/traffic-split2.t", 22},
		{"t/plugin/traffic-split3.t", 20},
		{"t/plugin/traffic-split4.t", 19},
		{"t/plugin/traffic-split5.t", 12},
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
	if len(manifest.Cases) != 94 {
		t.Fatalf("top-level cases = %d, want exactly 94", len(manifest.Cases))
	}

	next := make(map[string]int, len(wantSources))
	for _, source := range wantSources {
		next[source.file] = 1
	}
	seenNames := make(map[string]struct{}, len(manifest.Cases))
	for i, testCase := range manifest.Cases {
		want, ok := next[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != want {
			t.Errorf("case %d %q source tests = %v, want [%d]", i+1, testCase.Name, testCase.Source.Tests, want)
		}
		next[testCase.Source.File]++
		lowerName := strings.ToLower(strings.TrimSpace(testCase.Name))
		for _, forbidden := range []string{"placeholder", "generic", "probe", "source-", "block-"} {
			if strings.Contains(lowerName, forbidden) {
				t.Errorf("case %d has placeholder-like name %q", i+1, testCase.Name)
			}
		}
		if _, exists := seenNames[testCase.Name]; exists {
			t.Errorf("case %d repeats name %q", i+1, testCase.Name)
		}
		seenNames[testCase.Name] = struct{}{}
		if !containsTrafficSplitPluginConfig(testCase.Config) {
			t.Errorf("case %d %q has no route traffic-split configuration", i+1, testCase.Name)
		}
		if len(testCase.Fixtures) == 0 || len(testCase.Steps) == 0 {
			t.Errorf("case %d %q must execute real fixtures and request steps", i+1, testCase.Name)
		}
		for fixtureIndex, fixture := range testCase.Fixtures {
			httpFixture := (fixture.Kind == "http" || fixture.Kind == "https") && len(fixture.Respond) > 0
			networkFixture := fixture.Kind == "tcp" && len(fixture.NetworkRespond) > 0
			if fixture.Name == "" || (!httpFixture && !networkFixture) {
				t.Errorf("case %d %q fixture %d is not an executable HTTP fixture", i+1, testCase.Name, fixtureIndex+1)
			}
			if fixture.ExpectRequests == nil && len(fixture.Expect) == 0 && len(fixture.NetworkExpect) == 0 {
				t.Errorf("case %d %q fixture %d has no request assertion", i+1, testCase.Name, fixtureIndex+1)
			}
		}
		for stepIndex, step := range testCase.Steps {
			if step.Name == "" || step.Input["path"] == nil || step.Output["status"] == nil {
				t.Errorf("case %d %q step %d lacks a named request/status assertion", i+1, testCase.Name, stepIndex+1)
			}
		}
		encodedCase, err := yaml.Marshal(testCase)
		if err != nil {
			t.Fatalf("encode case %d %q for semantic checks: %v", i+1, testCase.Name, err)
		}
		assertTrafficSplitCaseSemantics(
			t,
			i+1,
			testCase.Name,
			testCase.Source.File,
			testCase.Source.Tests[0],
			string(encodedCase),
		)
	}
	for _, source := range wantSources {
		want := make([]int, source.tests)
		for i := range want {
			want[i] = i + 1
		}
		got := make([]int, next[source.file]-1)
		for i := range got {
			got[i] = i + 1
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s mapped tests = %v, want %v", source.file, got, want)
		}
	}
}

func assertTrafficSplitCaseSemantics(t *testing.T, index int, name, file string, testNumber int, encoded string) {
	t.Helper()
	required := []string{}
	switch file {
	case "t/plugin/traffic-split2.t":
		switch {
		case testNumber >= 4 && testNumber <= 9:
			required = []string{"pass_host"}
		case testNumber == 10 || testNumber == 11:
			required = []string{"chash", "hash_on", "key"}
		case testNumber >= 16 && testNumber <= 17:
			required = []string{"upstream_id", "upstreams:"}
		case testNumber == 20:
			required = []string{"upstream_id", "missing"}
		case testNumber >= 21:
			required = []string{"scheme: https", "kind: https"}
		}
	case "t/plugin/traffic-split4.t":
		required = []string{"upstream_id", "upstreams:"}
		if testNumber == 15 || testNumber == 16 {
			required = append(required, "checks:", "unhealthy:")
		}
		if testNumber >= 17 {
			required = append(
				required,
				"dead-one",
				"dead-two",
				"dead-three",
				"network_expect",
				"close: true",
			)
		}
	case "t/plugin/traffic-split5.t":
		switch {
		case testNumber == 7 || testNumber == 8:
			required = []string{"timeout:", "elapsed_less_than"}
		case testNumber >= 9 && testNumber <= 11:
			required = []string{"post_arg_id", "application/x-www-form-urlencoded", "body: id=1"}
		case testNumber == 12:
			required = []string{"upstream_id", "config_timeout", "config_probe"}
		}
	}
	for _, marker := range required {
		if !strings.Contains(encoded, marker) {
			t.Errorf("case %d %q lacks required behavioral marker %q", index, name, marker)
		}
	}
}

func containsTrafficSplitPluginConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if plugins, ok := typed["plugins"].(map[string]any); ok {
			if _, ok := plugins["traffic-split"]; ok {
				return true
			}
		}
		for _, child := range typed {
			if containsTrafficSplitPluginConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsTrafficSplitPluginConfig)
	}
	return false
}

func assertTrafficSplitManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertTrafficSplitManifestHasNoAliasesOrMerges(t, child)
	}
}
