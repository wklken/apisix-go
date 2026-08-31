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
