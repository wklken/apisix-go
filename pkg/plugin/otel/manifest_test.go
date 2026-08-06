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
		File        string `yaml:"file"`
		Tests       int    `yaml:"tests"`
		TestNumbers []int  `yaml:"test_numbers"`
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
	// testNumbers is nil when every pinned test 1..tests is converted. A
	// non-nil list names the exact converted upstream numbers, allowing
	// blocked gaps (opentelemetry.t TEST 26-28 serverless rewrite injection).
	// TEST 30 (opentelemetry.t) and TEST 6 (opentelemetry6.t) verify the
	// OpenResty inject_core_spans phase-level span tree (apisix.phase.*
	// spans with sni_radixtree_match/http_router_match/resolve_dns
	// children). The Go opentelemetry plugin emits a single request-scoped
	// span per request and has no phase-span instrumentation, so both
	// numbers are blocked_design in corpus_scope.yaml instead of converted.
	wantSources := []struct {
		file        string
		tests       int
		testNumbers []int
	}{
		{
			"t/plugin/opentelemetry.t",
			44,
			[]int{
				1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20,
				21, 22, 23, 24, 25, 29, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40,
				41, 42, 43, 44, 45, 46, 47, 48,
			},
		},
		{"t/plugin/opentelemetry2.t", 4, nil},
		{"t/plugin/opentelemetry4-bugfix-pb-state.t", 3, nil},
		{"t/plugin/opentelemetry5.t", 13, nil},
		{"t/plugin/opentelemetry6.t", 8, []int{1, 2, 3, 4, 5, 7, 8, 9}},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	total := 0
	sequence := make(map[string][]int, len(wantSources))
	for i, want := range wantSources {
		got := manifest.Sources[i]
		wantNumbers := want.testNumbers
		if wantNumbers == nil {
			wantNumbers = make([]int, want.tests)
			for n := range wantNumbers {
				wantNumbers[n] = n + 1
			}
		}
		if got.File != want.file || got.Tests != want.tests || !slices.Equal(got.TestNumbers, want.testNumbers) {
			t.Fatalf(
				"source %d = (%q, %d, %v), want (%q, %d, %v)",
				i+1, got.File, got.Tests, got.TestNumbers, want.file, want.tests, want.testNumbers,
			)
		}
		total += len(wantNumbers)
		sequence[want.file] = wantNumbers
	}
	if len(manifest.Cases) != total {
		t.Fatalf("top-level cases = %d, want %d pinned TEST blocks", len(manifest.Cases), total)
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
		if _, ok := sequence[testCase.Source.File]; !ok {
			t.Fatalf("case %d %q maps unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		mapped[testCase.Source.File] = append(mapped[testCase.Source.File], testCase.Source.Tests[0])
		if len(testCase.Fixtures) < 2 || len(testCase.Steps) == 0 {
			t.Fatalf("case %d %q lacks real upstream/collector fixtures or request steps", i+1, testCase.Name)
		}
		assertOpenTelemetryRuntime(t, i+1, testCase.Name, testCase.Runtime)
		assertOpenTelemetryRoute(t, i+1, testCase.Name, testCase.Config)
	}
	for file, want := range sequence {
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
