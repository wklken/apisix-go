package pluginintegration

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	pluginregistry "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"go.yaml.in/yaml/v3"
)

var manifestTargetPluginGroups = map[string][]string{
	"ai-proxy": {"ai-proxy-multi"},
}

func manifestYAMLFiles() ([]string, error) {
	return filepath.Glob("*.yaml")
}

func TestManifestCorpusValidates(t *testing.T) {
	files, err := manifestYAMLFiles()
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no manifests found")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := loadManifest(file, data); err != nil {
			t.Fatalf("load %s: %v", file, err)
		}
	}
}

func TestSAMLManifestHasIndependentSingletonCases(t *testing.T) {
	data, err := os.ReadFile("saml-auth.yaml")
	if err != nil {
		t.Fatalf("read saml-auth.yaml: %v", err)
	}
	if strings.Contains(string(data), "<<:") {
		t.Fatal("saml-auth.yaml uses YAML merge aliases instead of independent cases")
	}
	manifest, err := loadManifest("saml-auth.yaml", data)
	if err != nil {
		t.Fatalf("load saml-auth.yaml: %v", err)
	}
	wantByFile, wantCases := selectedSourceTestsByFile(manifest)
	if got := len(manifest.Cases); got != wantCases {
		t.Fatalf("SAML cases = %d, want %d independent selected source cases", got, wantCases)
	}
	gotByFile := make(map[string][]int, len(wantByFile))
	for _, testCase := range manifest.Cases {
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf(
				"case %q source tests = %v, want one source block",
				testCase.Name,
				testCase.Source.Tests,
			)
		}
		if _, ok := wantByFile[testCase.Source.File]; !ok {
			t.Fatalf("case %q has unexpected source %q", testCase.Name, testCase.Source.File)
		}
		gotByFile[testCase.Source.File] = append(gotByFile[testCase.Source.File], testCase.Source.Tests[0])
	}
	for file, want := range wantByFile {
		got := gotByFile[file]
		sort.Ints(got)
		want = slices.Clone(want)
		sort.Ints(want)
		if !slices.Equal(got, want) {
			t.Fatalf("%s tests = %v, want selected tests %v", file, got, want)
		}
	}
}

func selectedSourceTestsByFile(manifest *Manifest) (map[string][]int, int) {
	testsByFile := make(map[string][]int)
	total := 0
	for _, source := range manifestSources(manifest) {
		numbers := sourceTestNumbers(source)
		testsByFile[source.File] = append(testsByFile[source.File], numbers...)
		total += len(numbers)
	}
	return testsByFile, total
}

func TestAIRateLimitingManifestMapsExactlyOnePinnedBlockPerBehavioralCase(t *testing.T) {
	const manifestFile = "ai-rate-limiting.yaml"
	data, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatalf("read %s: %v", manifestFile, err)
	}
	if strings.Contains(string(data), "<<:") {
		t.Fatalf("%s contains YAML merge keys", manifestFile)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", manifestFile, err)
	}
	if node := firstYAMLAnchorOrAlias(&document); node != nil {
		t.Fatalf("%s contains YAML anchor or alias %q", manifestFile, node.Value)
	}
	manifest, err := loadManifest(manifestFile, data)
	if err != nil {
		t.Fatalf("load %s: %v", manifestFile, err)
	}
	wantByFile, wantCases := selectedSourceTestsByFile(manifest)
	if got := len(manifest.Cases); got != wantCases {
		t.Fatalf(
			"%s top-level cases = %d, want exactly %d selected pinned behavioral cases",
			manifestFile,
			got,
			wantCases,
		)
	}

	next := make(map[string]int, len(wantByFile))
	for i, testCase := range manifest.Cases {
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf(
				"%s case %d %q maps %d source blocks, want exactly one",
				manifestFile,
				i+1,
				testCase.Name,
				len(testCase.Source.Tests),
			)
		}
		wantTests, ok := wantByFile[testCase.Source.File]
		if !ok {
			t.Fatalf("%s case %d has unexpected source %q", manifestFile, i+1, testCase.Source.File)
		}
		nextIndex := next[testCase.Source.File]
		if nextIndex >= len(wantTests) {
			t.Fatalf("%s case %d duplicates exhausted source %q", manifestFile, i+1, testCase.Source.File)
		}
		want := wantTests[nextIndex]
		if got := testCase.Source.Tests[0]; got != want {
			t.Fatalf(
				"%s case %d %q maps source test %d, want next source test %d",
				manifestFile,
				i+1,
				testCase.Name,
				got,
				want,
			)
		}
		next[testCase.Source.File]++
	}
	for file, want := range wantByFile {
		if got := next[file]; got != len(want) {
			t.Fatalf("%s mapped %d selected tests, want %d", file, got, len(want))
		}
	}
}

func firstYAMLAnchorOrAlias(node *yaml.Node) *yaml.Node {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return node
	}
	for _, child := range node.Content {
		if found := firstYAMLAnchorOrAlias(child); found != nil {
			return found
		}
	}
	return nil
}

func manifestSources(manifest *Manifest) []SourceSpec {
	if len(manifest.Sources) > 0 {
		return manifest.Sources
	}
	return []SourceSpec{manifest.Source}
}

func TestManifestExercisesTargetPlugin(t *testing.T) {
	tests := []struct {
		name     string
		manifest *Manifest
		plugin   string
		want     bool
	}{
		{
			name: "route plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"acl": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   true,
		},
		{
			name: "global plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"global_rules": []any{map[string]any{"plugins": map[string]any{"error-page": map[string]any{}}}},
			}}}},
			plugin: "error-page",
			want:   true,
		},
		{
			name: "control plugin",
			manifest: &Manifest{Cases: []Case{{Runtime: map[string]any{
				"plugins": []any{"node-status"},
			}}}},
			plugin: "node-status",
			want:   true,
		},
		{
			name: "variant plugin",
			manifest: &Manifest{Cases: []Case{{Variants: []CaseVariant{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"ua-restriction": map[string]any{}}}},
			}}}}}},
			plugin: "ua-restriction",
			want:   true,
		},
		{
			name: "explicit manifest target alias",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"ai-proxy-multi": map[string]any{}}}},
			}}}},
			plugin: "ai-proxy",
			want:   true,
		},
		{
			name: "canonical factory alias",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"otel": map[string]any{}}}},
			}}}},
			plugin: "opentelemetry",
			want:   true,
		},
		{
			name: "step config plugin",
			manifest: &Manifest{Cases: []Case{{
				Config: map[string]any{
					"routes": []any{map[string]any{"plugins": map[string]any{"mocking": map[string]any{}}}},
				},
				Steps: []CaseStep{{Config: map[string]any{
					"routes": []any{map[string]any{"plugins": map[string]any{"ai-proxy": map[string]any{}}}},
				}}},
			}}},
			plugin: "ai-proxy",
			want:   true,
		},
		{
			name: "manifest target alias is narrow",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"ai-proxy-multi": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   false,
		},
		{
			name: "fixture proxy placeholder",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"uri": "/*", "upstream": map[string]any{}}},
			}}}},
			plugin: "saml-auth",
			want:   false,
		},
		{
			name: "unrelated plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"mocking": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pluginNames := targetPluginActivationNames(t, test.plugin)
			if got := manifestExercisesPlugin(test.manifest, pluginNames); got != test.want {
				t.Fatalf("manifestExercisesPlugin() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestManifestCorpusExercisesTargetPlugins(t *testing.T) {
	files, err := manifestYAMLFiles()
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	for _, file := range files {
		pluginName := manifestPluginName(file)
		t.Run(pluginName, func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			manifest, err := loadManifest(file, data)
			if err != nil {
				t.Fatalf("load %s: %v", file, err)
			}
			assertManifestExercisesTargetPlugin(t, file, manifest, pluginName)
		})
	}
}

func manifestPluginName(file string) string {
	pluginName := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if pluginName == "redirect2" {
		return "redirect"
	}
	return pluginName
}

func assertManifestExercisesTargetPlugin(t *testing.T, file string, manifest *Manifest, pluginName string) {
	t.Helper()
	pluginNames := targetPluginActivationNames(t, pluginName)
	if !manifestExercisesPlugin(manifest, pluginNames) {
		t.Errorf("%s never activates target plugin %q", file, pluginName)
	}
	for caseIndex := range manifest.Cases {
		caseSpec := &manifest.Cases[caseIndex]
		if len(caseSpec.Variants) == 0 {
			activates := caseExercisesTargetPlugin(
				caseSpec.Runtime,
				caseSpec.Config,
				caseSpec.Steps,
				pluginNames,
			)
			if err := validateTargetPluginExemption(caseSpec.TargetPluginExemptReason, activates); err != nil {
				t.Errorf("%s case %q target plugin %q: %v", file, caseSpec.Name, pluginName, err)
			}
			continue
		}
		for variantIndex := range caseSpec.Variants {
			variant := &caseSpec.Variants[variantIndex]
			activates := caseExercisesTargetPlugin(
				variant.Runtime,
				variant.Config,
				variant.Steps,
				pluginNames,
			)
			if err := validateTargetPluginExemption(variant.TargetPluginExemptReason, activates); err != nil {
				t.Errorf(
					"%s case %q variant %q target plugin %q: %v",
					file,
					caseSpec.Name,
					variant.Name,
					pluginName,
					err,
				)
			}
		}
	}
}

func validateTargetPluginExemption(reason string, activates bool) error {
	if activates {
		if strings.TrimSpace(reason) != "" {
			return fmt.Errorf("target_plugin_exempt_reason must be empty when the target plugin is activated")
		}
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("target_plugin_exempt_reason is required when the target plugin is not activated")
	}
	return nil
}

func TestTargetPluginExemptionRequiresReasonForInactiveScenario(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{name: "missing reason", want: "target_plugin_exempt_reason is required"},
		{name: "blank reason", reason: "  \t", want: "target_plugin_exempt_reason is required"},
		{name: "valid reason", reason: "intentional negative coverage case"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTargetPluginExemption(tt.reason, false)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validateTargetPluginExemption() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateTargetPluginExemption() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestTargetPluginExemptionRejectedWhenScenarioActivatesTarget(t *testing.T) {
	tests := []struct {
		name     string
		runtime  map[string]any
		config   map[string]any
		steps    []CaseStep
		exempted string
	}{
		{
			name: "case",
			config: map[string]any{
				"routes": []any{map[string]any{
					"plugins": map[string]any{"error-log-logger": map[string]any{}},
				}},
			},
			exempted: "stale exemption",
		},
		{
			name: "variant",
			runtime: map[string]any{
				"plugins": []any{"error-log-logger"},
			},
			exempted: "stale exemption",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pluginNames := targetPluginActivationNames(t, "error-log-logger")
			if !caseExercisesTargetPlugin(tt.runtime, tt.config, tt.steps, pluginNames) {
				t.Fatal("caseExercisesTargetPlugin() = false, want active target plugin")
			}
			err := validateTargetPluginExemption(tt.exempted, true)
			if err == nil || !strings.Contains(err.Error(), "must be empty") {
				t.Fatalf("validateTargetPluginExemption() error = %v, want stale exemption rejection", err)
			}
		})
	}
}

func targetPluginActivationNames(t *testing.T, pluginName string) []string {
	t.Helper()
	target := pluginregistry.New(pluginName, base.Dependencies{})
	if target == nil {
		t.Fatalf("registered plugin %q is missing", pluginName)
	}
	if err := target.Init(); err != nil {
		t.Fatalf("initialize registered plugin %q: %v", pluginName, err)
	}
	names := make([]string, 0, 2)
	for _, definition := range pluginregistry.Definitions() {
		instance := pluginregistry.New(definition.Factory, base.Dependencies{})
		if instance == nil {
			t.Fatalf("registered factory %q has no constructor", definition.Factory)
		}
		if err := instance.Init(); err != nil {
			t.Fatalf("initialize registered factory %q: %v", definition.Factory, err)
		}
		if instance.GetName() == target.GetName() {
			names = append(names, definition.Factory)
		}
	}
	names = append(names, manifestTargetPluginGroups[pluginName]...)
	sort.Strings(names)
	return slices.Compact(names)
}

func manifestExercisesPlugin(manifest *Manifest, pluginNames []string) bool {
	for i := range manifest.Cases {
		caseSpec := &manifest.Cases[i]
		if caseExercisesTargetPlugin(caseSpec.Runtime, caseSpec.Config, caseSpec.Steps, pluginNames) {
			return true
		}
		for j := range caseSpec.Variants {
			variant := &caseSpec.Variants[j]
			if caseExercisesTargetPlugin(variant.Runtime, variant.Config, variant.Steps, pluginNames) {
				return true
			}
		}
	}
	return false
}

func caseExercisesTargetPlugin(
	runtime, config map[string]any,
	steps []CaseStep,
	pluginNames []string,
) bool {
	if scenarioExercisesTargetPlugin(runtime, config, pluginNames) {
		return true
	}
	for i := range steps {
		if scenarioExercisesTargetPlugin(nil, steps[i].Config, pluginNames) {
			return true
		}
	}
	return false
}

func scenarioExercisesTargetPlugin(runtime, config map[string]any, pluginNames []string) bool {
	for _, candidate := range pluginNames {
		if scenarioExercisesPlugin(runtime, config, candidate) {
			return true
		}
	}
	return false
}

func scenarioExercisesPlugin(runtime, config map[string]any, pluginName string) bool {
	switch plugins := runtime["plugins"].(type) {
	case []any:
		for _, configured := range plugins {
			if configured == pluginName {
				return true
			}
		}
	case []string:
		if slices.Contains(plugins, pluginName) {
			return true
		}
	}
	return configContainsPlugin(config, pluginName)
}

func configContainsPlugin(value any, pluginName string) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			if key == "plugins" {
				if plugins, ok := nested.(map[string]any); ok {
					if _, configured := plugins[pluginName]; configured {
						return true
					}
				}
			}
			if configContainsPlugin(nested, pluginName) {
				return true
			}
		}
	case []any:
		for _, nested := range current {
			if configContainsPlugin(nested, pluginName) {
				return true
			}
		}
	}
	return false
}
