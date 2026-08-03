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
		File  string `yaml:"file"`
		Tests int    `yaml:"tests"`
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
	wantSources := map[string]int{
		"t/plugin/log-rotate.t":  6,
		"t/plugin/log-rotate2.t": 5,
		"t/plugin/log-rotate3.t": 6,
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	for _, source := range manifest.Sources {
		if want := wantSources[source.File]; source.Tests != want {
			t.Fatalf("source %s tests = %d, want %d", source.File, source.Tests, want)
		}
	}
	if len(manifest.Cases) != 17 {
		t.Fatalf("top-level cases = %d, want 17 pinned TEST blocks", len(manifest.Cases))
	}

	mapped := make(map[string][]int, len(wantSources))
	for i, testCase := range manifest.Cases {
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf("case %d %q maps tests %v, want exactly one", i+1, testCase.Name, testCase.Source.Tests)
		}
		if _, ok := wantSources[testCase.Source.File]; !ok {
			t.Fatalf("case %d %q maps unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		mapped[testCase.Source.File] = append(mapped[testCase.Source.File], testCase.Source.Tests[0])
		if len(testCase.Fixtures) == 0 || len(testCase.Steps) == 0 || len(testCase.After) == 0 {
			t.Fatalf("case %d %q lacks fixture, request steps, or file assertions", i+1, testCase.Name)
		}
		assertLogRotateRuntimeConfiguration(t, i+1, testCase.Name, testCase.Runtime)
		assertLogRotateStandaloneRoute(t, i+1, testCase.Name, testCase.Config)
	}
	for file, count := range wantSources {
		want := make([]int, count)
		for i := range want {
			want[i] = i + 1
		}
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
}

func assertLogRotateStandaloneRoute(t *testing.T, index int, name string, config map[string]any) {
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
	configured, ok := plugins["log-rotate"].(map[string]any)
	if !ok || configured["access_log"] == nil || configured["error_log"] == nil {
		t.Fatalf("case %d %q lacks real log-rotate log paths", index, name)
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
