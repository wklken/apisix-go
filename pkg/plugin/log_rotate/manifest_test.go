package log_rotate

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

type logRotateManifest struct {
	Sources []struct {
		Commit         string `yaml:"commit"`
		File           string `yaml:"file"`
		Tests          int    `yaml:"tests"`
		TestNumbers    []int  `yaml:"test_numbers"`
		RegressionOnly bool   `yaml:"regression_only"`
	} `yaml:"sources"`
	Cases []struct {
		Name     string          `yaml:"name"`
		Source   logRotateSource `yaml:"source"`
		Runtime  map[string]any  `yaml:"runtime"`
		Config   map[string]any  `yaml:"config"`
		Fixtures []any           `yaml:"fixtures"`
		Steps    []any           `yaml:"steps"`
		After    []any           `yaml:"after_shutdown"`
	} `yaml:"cases"`
}

type logRotateSource struct {
	File  string `yaml:"file"`
	Tests []int  `yaml:"tests"`
}

func TestManifestMapsEveryPinnedBlockToIndependentRealProcessCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "log-rotate.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log-rotate manifest: %v", err)
	}
	if bytes.Contains(data, []byte("<<:")) {
		t.Fatal("log-rotate manifest contains a YAML merge key")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode log-rotate YAML syntax tree: %v", err)
	}
	if node := firstLogRotateAnchorOrAlias(&document); node != nil {
		t.Fatalf("log-rotate manifest contains YAML anchor or alias %q", node.Value)
	}

	var manifest logRotateManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode log-rotate manifest: %v", err)
	}
	wantSources := []struct {
		file       string
		tests      int
		numbers    []int
		regression bool
	}{
		{file: "t/plugin/log-rotate.t", tests: 4, numbers: []int{1, 2, 3, 5}},
		{file: "t/plugin/log-rotate.t", tests: 1, numbers: []int{6}, regression: true},
		{file: "t/plugin/log-rotate2.t", tests: 5},
		{file: "t/plugin/log-rotate3.t", tests: 6},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	for index, want := range wantSources {
		source := manifest.Sources[index]
		if source.File != want.file || source.Tests != want.tests ||
			!slices.Equal(source.TestNumbers, want.numbers) || source.RegressionOnly != want.regression {
			t.Fatalf("source %d = %#v, want %#v", index+1, source, want)
		}
	}
	if len(manifest.Cases) != 16 {
		t.Fatalf("top-level cases = %d, want 16 real-process TEST blocks", len(manifest.Cases))
	}

	wantMappings := map[string][]int{
		"t/plugin/log-rotate.t":  {1, 2, 3, 5, 6},
		"t/plugin/log-rotate2.t": {1, 2, 3, 4, 5},
		"t/plugin/log-rotate3.t": {1, 2, 3, 4, 5, 6},
	}
	mapped := make(map[string][]int, len(wantMappings))
	for i, testCase := range manifest.Cases {
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf("case %d %q maps tests %v, want exactly one", i+1, testCase.Name, testCase.Source.Tests)
		}
		if _, ok := wantMappings[testCase.Source.File]; !ok {
			t.Fatalf("case %d %q maps unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		mapped[testCase.Source.File] = append(mapped[testCase.Source.File], testCase.Source.Tests[0])
		if len(testCase.Fixtures) == 0 || len(testCase.Steps) == 0 || len(testCase.After) == 0 {
			t.Fatalf("case %d %q lacks fixture, request steps, or file assertions", i+1, testCase.Name)
		}
		assertLogRotateRuntimeConfiguration(t, i+1, testCase.Name, testCase.Runtime)
		assertLogRotateSystemRoute(t, i+1, testCase.Name, testCase.Config)
	}
	for file, want := range wantMappings {
		if !slices.Equal(mapped[file], want) {
			t.Fatalf("source %s mappings = %v, want %v", file, mapped[file], want)
		}
	}
}

func assertLogRotateRuntimeConfiguration(t *testing.T, index int, name string, runtime map[string]any) {
	t.Helper()
	pluginAttr, ok := runtime["plugin_attr"].(map[string]any)
	if !ok {
		t.Fatalf("case %d %q lacks runtime plugin_attr", index, name)
	}
	if _, ok := pluginAttr["log-rotate"].(map[string]any); !ok {
		t.Fatalf("case %d %q lacks log-rotate plugin_attr", index, name)
	}
	plugins, ok := runtime["plugins"].([]any)
	if !ok || !slices.Contains(plugins, any("log-rotate")) {
		t.Fatalf("case %d %q lacks runtime log-rotate enablement", index, name)
	}
}

func assertLogRotateSystemRoute(t *testing.T, index int, name string, config map[string]any) {
	t.Helper()
	routes, ok := config["routes"].([]any)
	if !ok || len(routes) == 0 {
		t.Fatalf("case %d %q lacks standalone routes", index, name)
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("case %d %q first route has unexpected type %T", index, name, routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("case %d %q lacks route plugins", index, name)
	}
	if _, configured := plugins["log-rotate"]; configured {
		t.Fatalf("case %d %q configures system-only log-rotate on a route", index, name)
	}
	if _, ok := plugins["file-logger"].(map[string]any); !ok {
		t.Fatalf("case %d %q lacks the file-logger traffic source", index, name)
	}
}

func firstLogRotateAnchorOrAlias(node *yaml.Node) *yaml.Node {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return node
	}
	for _, child := range node.Content {
		if found := firstLogRotateAnchorOrAlias(child); found != nil {
			return found
		}
	}
	return nil
}
