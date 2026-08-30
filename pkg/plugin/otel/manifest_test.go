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
		Commit         string `yaml:"commit"`
		File           string `yaml:"file"`
		Tests          int    `yaml:"tests"`
		TestNumbers    []int  `yaml:"test_numbers"`
		RegressionOnly bool   `yaml:"regression_only"`
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
	File        string `yaml:"file"`
	Tests       []int  `yaml:"tests"`
	LocalReason string `yaml:"local_reason"`
}

func TestManifestHasDirectOtelAliasActivationCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "opentelemetry.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read opentelemetry manifest: %v", err)
	}
	var manifest opentelemetryManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode opentelemetry manifest: %v", err)
	}

	const name = "otel-alias-direct-activation-produces-span"
	for i, testCase := range manifest.Cases {
		if testCase.Name != name {
			continue
		}
		if strings.TrimSpace(testCase.Source.LocalReason) == "" {
			t.Fatalf("case %q lacks a local evidence reason", name)
		}
		if len(testCase.Fixtures) < 2 || len(testCase.Steps) == 0 {
			t.Fatalf("case %q lacks real upstream/collector fixtures or request steps", name)
		}
		assertOpenTelemetryRuntimeKey(t, i+1, name, testCase.Runtime, "otel")
		assertOpenTelemetryAllowlistKey(t, i+1, name, testCase.Runtime, "otel")
		assertOpenTelemetryRouteKey(t, i+1, name, testCase.Config, "otel")
		return
	}
	t.Fatalf("manifest lacks direct activation case %q", name)
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
	// testNumbers is nil when every pinned test 1..tests is converted. The
	// APISIX 3.17 source owns TEST 1-25; later source blocks remain explicit
	// regression-only evidence instead of being attributed to the target.
	// A non-nil list names the exact upstream numbers, allowing blocked gaps
	// (opentelemetry.t TEST 26-28 serverless rewrite injection).
	// TEST 30 (opentelemetry.t) and TEST 6 (opentelemetry6.t) verify the
	// OpenResty inject_core_spans phase-level span tree (apisix.phase.*
	// spans with sni_radixtree_match/http_router_match/resolve_dns
	// children). The Go opentelemetry plugin emits a single request-scoped
	// span per request and has no phase-span instrumentation, so both
	// numbers are not converted into standalone cases.
	wantSources := []struct {
		commit         string
		file           string
		tests          int
		testNumbers    []int
		regressionOnly bool
	}{
		{
			"9ef2ecab67f652d38365049613610ef649bb4ad0",
			"t/plugin/opentelemetry.t",
			25,
			[]int{
				1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20,
				21, 22, 23, 24, 25,
			},
			false,
		},
		{
			"c3d7d5ec69774121f53d2e20d29d09c816795dd7",
			"t/plugin/opentelemetry.t",
			19,
			[]int{
				29, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40,
				41, 42, 43, 44, 45, 46, 47, 48,
			},
			true,
		},
		{"9ef2ecab67f652d38365049613610ef649bb4ad0", "t/plugin/opentelemetry2.t", 4, nil, false},
		{"9ef2ecab67f652d38365049613610ef649bb4ad0", "t/plugin/opentelemetry4-bugfix-pb-state.t", 3, nil, false},
		{"9ef2ecab67f652d38365049613610ef649bb4ad0", "t/plugin/opentelemetry5.t", 13, nil, false},
		{
			"9ef2ecab67f652d38365049613610ef649bb4ad0",
			"t/plugin/opentelemetry6.t", 8,
			[]int{1, 2, 3, 4, 5, 7, 8, 9},
			false,
		},
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
		if got.Commit != want.commit || got.File != want.file || got.Tests != want.tests ||
			!slices.Equal(got.TestNumbers, want.testNumbers) || got.RegressionOnly != want.regressionOnly {
			t.Fatalf(
				"source %d = (%q, %q, %d, %v, regression=%t), want (%q, %q, %d, %v, regression=%t)",
				i+1, got.Commit, got.File, got.Tests, got.TestNumbers, got.RegressionOnly,
				want.commit, want.file, want.tests, want.testNumbers, want.regressionOnly,
			)
		}
		total += len(wantNumbers)
		sequence[want.file] = append(sequence[want.file], wantNumbers...)
	}
	mapped := make(map[string][]int, len(wantSources))
	mappedCases := 0
	for i, testCase := range manifest.Cases {
		if strings.TrimSpace(testCase.Source.LocalReason) != "" {
			continue
		}
		mappedCases++
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
	if mappedCases != total {
		t.Fatalf("mapped cases = %d, want %d pinned TEST blocks", mappedCases, total)
	}
	for file, want := range sequence {
		if !slices.Equal(mapped[file], want) {
			t.Fatalf("source %s mappings = %v, want %v", file, mapped[file], want)
		}
	}
}

func assertOpenTelemetryRuntime(t *testing.T, index int, name string, runtime map[string]any) {
	assertOpenTelemetryRuntimeKey(t, index, name, runtime, "opentelemetry")
}

func assertOpenTelemetryRuntimeKey(
	t *testing.T,
	index int,
	name string,
	runtime map[string]any,
	pluginName string,
) {
	t.Helper()
	pluginAttr, ok := runtime["plugin_attr"].(map[string]any)
	if !ok {
		t.Fatalf("case %d %q lacks runtime plugin_attr", index, name)
	}
	attr, ok := pluginAttr[pluginName].(map[string]any)
	if !ok {
		t.Fatalf("case %d %q lacks %s runtime metadata", index, name, pluginName)
	}
	collector, ok := attr["collector"].(map[string]any)
	if !ok || collector["address"] == nil {
		t.Fatalf("case %d %q lacks real collector address", index, name)
	}
}

func assertOpenTelemetryAllowlistKey(
	t *testing.T,
	index int,
	name string,
	runtime map[string]any,
	pluginName string,
) {
	t.Helper()
	plugins, ok := runtime["plugins"].([]any)
	if !ok {
		t.Fatalf("case %d %q lacks runtime plugin allowlist", index, name)
	}
	for _, configured := range plugins {
		if configured == pluginName {
			return
		}
	}
	t.Fatalf("case %d %q runtime allowlist lacks %s", index, name, pluginName)
}

func assertOpenTelemetryRoute(t *testing.T, index int, name string, config map[string]any) {
	assertOpenTelemetryRouteKey(t, index, name, config, "opentelemetry")
}

func assertOpenTelemetryRouteKey(
	t *testing.T,
	index int,
	name string,
	config map[string]any,
	pluginName string,
) {
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
		if configured, ok := plugins[pluginName].(map[string]any); ok && configured != nil {
			return
		}
	}
	t.Fatalf("case %d %q has no route that configures %s", index, name, pluginName)
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
