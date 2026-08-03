package otel

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type opentelemetryManifest struct {
	Sources []struct {
		File  string `yaml:"file"`
		Tests int    `yaml:"tests"`
	} `yaml:"sources"`
	Cases []struct {
		Name     string                      `yaml:"name"`
		Source   opentelemetryManifestSource `yaml:"source"`
		Runtime  map[string]any              `yaml:"runtime"`
		Config   map[string]any              `yaml:"config"`
		Fixtures []any                       `yaml:"fixtures"`
		Steps    []any                       `yaml:"steps"`
	} `yaml:"cases"`
}

type opentelemetryManifestSource struct {
	File  string `yaml:"file"`
	Tests []int  `yaml:"tests"`
}

func TestManifestMapsEveryPinnedBlockToIndependentRealProcessCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "opentelemetry.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read opentelemetry manifest: %v", err)
	}
	if bytes.Contains(data, []byte("<<:")) {
		t.Fatal("opentelemetry manifest contains a YAML merge key")
	}
	for _, placeholder := range []string{
		"opentelemetry-source-",
		"/probe",
		"otlp-trace-is-exported",
		"source-complete skip",
	} {
		if bytes.Contains(data, []byte(placeholder)) {
			t.Fatalf("opentelemetry manifest contains generic placeholder %q", placeholder)
		}
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode opentelemetry YAML syntax tree: %v", err)
	}
	if node := firstOpenTelemetryAnchorOrAlias(&document); node != nil {
		t.Fatalf("opentelemetry manifest contains YAML anchor or alias %q", node.Value)
	}

	var manifest opentelemetryManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode opentelemetry manifest: %v", err)
	}
	wantSources := map[string]int{
		"t/plugin/opentelemetry.t":                  48,
		"t/plugin/opentelemetry2.t":                 4,
		"t/plugin/opentelemetry3.t":                 4,
		"t/plugin/opentelemetry4-bugfix-pb-state.t": 3,
		"t/plugin/opentelemetry5.t":                 13,
		"t/plugin/opentelemetry6.t":                 9,
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	for _, source := range manifest.Sources {
		want, ok := wantSources[source.File]
		if !ok {
			t.Fatalf("unknown source %q", source.File)
		}
		if source.Tests != want {
			t.Fatalf("source %s tests = %d, want %d", source.File, source.Tests, want)
		}
	}
	if len(manifest.Cases) != 81 {
		t.Fatalf("top-level cases = %d, want 81 pinned TEST blocks", len(manifest.Cases))
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
			t.Fatalf("case %d %q lacks real upstream/collector fixtures or request steps", i+1, testCase.Name)
		}
		assertOpenTelemetryRuntime(t, i+1, testCase.Name, testCase.Runtime)
		assertOpenTelemetryRoute(t, i+1, testCase.Name, testCase.Config)
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

func assertOpenTelemetryRuntime(t *testing.T, index int, name string, runtime map[string]any) {
	t.Helper()
	pluginAttr, ok := runtime["plugin_attr"].(map[string]any)
	if !ok {
		t.Fatalf("case %d %q lacks runtime plugin_attr", index, name)
	}
	attr, ok := pluginAttr["opentelemetry"].(map[string]any)
	if !ok {
		t.Fatalf("case %d %q lacks opentelemetry runtime metadata", index, name)
	}
	collector, ok := attr["collector"].(map[string]any)
	if !ok || collector["address"] == nil {
		t.Fatalf("case %d %q lacks real collector address", index, name)
	}
}

func assertOpenTelemetryRoute(t *testing.T, index int, name string, config map[string]any) {
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
		if configured, ok := plugins["opentelemetry"].(map[string]any); ok && configured != nil {
			return
		}
	}
	t.Fatalf("case %d %q has no route that configures opentelemetry", index, name)
}

func firstOpenTelemetryAnchorOrAlias(node *yaml.Node) *yaml.Node {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return node
	}
	for _, child := range node.Content {
		if found := firstOpenTelemetryAnchorOrAlias(child); found != nil {
			return found
		}
	}
	return nil
}
