package skywalking

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type skywalkingManifest struct {
	Sources []struct {
		File  string `yaml:"file"`
		Tests int    `yaml:"tests"`
	} `yaml:"sources"`
	Cases []struct {
		Name     string                   `yaml:"name"`
		Source   skywalkingManifestSource `yaml:"source"`
		Runtime  map[string]any           `yaml:"runtime"`
		Config   map[string]any           `yaml:"config"`
		Fixtures []any                    `yaml:"fixtures"`
		Steps    []any                    `yaml:"steps"`
	} `yaml:"cases"`
}

type skywalkingManifestSource struct {
	File  string `yaml:"file"`
	Tests []int  `yaml:"tests"`
}

func TestManifestMapsEveryPinnedBlockToIndependentRealProcessCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "skywalking.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read skywalking manifest: %v", err)
	}
	if bytes.Contains(data, []byte("<<:")) {
		t.Fatal("skywalking manifest contains a YAML merge key")
	}
	for _, placeholder := range []string{"skywalking-source-", "/probe", "log-delivered-to-sink"} {
		if bytes.Contains(data, []byte(placeholder)) {
			t.Fatalf("skywalking manifest contains generic placeholder %q", placeholder)
		}
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode skywalking YAML syntax tree: %v", err)
	}
	if node := firstSkyWalkingAnchorOrAlias(&document); node != nil {
		t.Fatalf("skywalking manifest contains YAML anchor or alias %q", node.Value)
	}

	var manifest skywalkingManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode skywalking manifest: %v", err)
	}
	wantSources := map[string]int{
		"t/plugin/skywalking.t":  15,
		"t/plugin/skywalking2.t": 2,
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
		if strings.TrimSpace(testCase.Name) == "" || len(testCase.Source.Tests) != 1 {
			t.Fatalf(
				"case %d name/source = %q/%v, want named singleton source",
				i+1,
				testCase.Name,
				testCase.Source.Tests,
			)
		}
		if _, ok := wantSources[testCase.Source.File]; !ok {
			t.Fatalf("case %d %q maps unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		mapped[testCase.Source.File] = append(mapped[testCase.Source.File], testCase.Source.Tests[0])
		if len(testCase.Fixtures) < 2 || len(testCase.Steps) == 0 {
			t.Fatalf("case %d %q lacks real upstream/sink fixtures or request steps", i+1, testCase.Name)
		}
		assertSkyWalkingRuntime(t, i+1, testCase.Name, testCase.Runtime)
		assertSkyWalkingRoute(t, i+1, testCase.Name, testCase.Config)
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

func assertSkyWalkingRuntime(t *testing.T, index int, name string, runtime map[string]any) {
	t.Helper()
	pluginAttr, ok := runtime["plugin_attr"].(map[string]any)
	if !ok {
		t.Fatalf("case %d %q lacks runtime plugin_attr", index, name)
	}
	attr, ok := pluginAttr["skywalking"].(map[string]any)
	if !ok || attr["endpoint_addr"] == nil || attr["report_interval"] == nil {
		t.Fatalf("case %d %q lacks real skywalking endpoint/report configuration", index, name)
	}
}

func assertSkyWalkingRoute(t *testing.T, index int, name string, config map[string]any) {
	t.Helper()
	routes, ok := config["routes"].([]any)
	if !ok || len(routes) == 0 {
		t.Fatalf("case %d %q lacks standalone routes", index, name)
	}
	for _, rawRoute := range routes {
		route, ok := rawRoute.(map[string]any)
		if !ok {
			continue
		}
		plugins, ok := route["plugins"].(map[string]any)
		if !ok {
			continue
		}
		if configured, ok := plugins["skywalking"].(map[string]any); ok && configured != nil {
			return
		}
	}
	t.Fatalf("case %d %q has no route that configures skywalking", index, name)
}

func firstSkyWalkingAnchorOrAlias(node *yaml.Node) *yaml.Node {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return node
	}
	for _, child := range node.Content {
		if found := firstSkyWalkingAnchorOrAlias(child); found != nil {
			return found
		}
	}
	return nil
}
