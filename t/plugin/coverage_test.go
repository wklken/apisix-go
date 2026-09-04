package pluginintegration

import (
	"fmt"
	"net/url"
	"os"
	pathpkg "path"
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
			manifest: &Manifest{Cases: []Case{{Input: HTTPInput{Path: "/"}, Config: map[string]any{
				"routes": []any{map[string]any{"uri": "/", "plugins": map[string]any{"acl": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   true,
		},
		{
			name: "global plugin",
			manifest: &Manifest{Cases: []Case{{Input: HTTPInput{Path: "/"}, Config: map[string]any{
				"global_rules": []any{map[string]any{"plugins": map[string]any{"error-page": map[string]any{}}}},
			}}}},
			plugin: "error-page",
			want:   true,
		},
		{
			name: "control plugin",
			manifest: &Manifest{Cases: []Case{{Input: HTTPInput{Path: "/apisix/status"}, Runtime: map[string]any{
				"plugins": []any{"node-status"},
			}}}},
			plugin: "node-status",
			want:   true,
		},
		{
			name: "variant plugin",
			manifest: &Manifest{
				Cases: []Case{{Variants: []CaseVariant{{Input: HTTPInput{Path: "/"}, Config: map[string]any{
					"routes": []any{
						map[string]any{"uri": "/", "plugins": map[string]any{"ua-restriction": map[string]any{}}},
					},
				}}}}},
			},
			plugin: "ua-restriction",
			want:   true,
		},
		{
			name: "explicit manifest target alias",
			manifest: &Manifest{Cases: []Case{{Input: HTTPInput{Path: "/"}, Config: map[string]any{
				"routes": []any{
					map[string]any{"uri": "/", "plugins": map[string]any{"ai-proxy-multi": map[string]any{}}},
				},
			}}}},
			plugin: "ai-proxy",
			want:   true,
		},
		{
			name: "step config plugin",
			manifest: &Manifest{Cases: []Case{{
				Config: map[string]any{
					"routes": []any{map[string]any{"uri": "/", "plugins": map[string]any{"mocking": map[string]any{}}}},
				},
				Steps: []CaseStep{{Config: map[string]any{
					"routes": []any{
						map[string]any{"uri": "/", "plugins": map[string]any{"ai-proxy": map[string]any{}}},
					},
				}, Input: HTTPInput{Path: "/"}}},
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
		{
			name: "target plugin on an unrequested route",
			manifest: &Manifest{Cases: []Case{{
				Input: HTTPInput{Path: "/requested"},
				Config: map[string]any{"routes": []any{
					map[string]any{"id": "requested", "uri": "/requested"},
					map[string]any{
						"id": "unused", "uri": "/unused",
						"plugins": map[string]any{"acl": map[string]any{}},
					},
				}},
			}}},
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
				caseSpec.Input,
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
				variant.Input,
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
		if caseExercisesTargetPlugin(
			caseSpec.Runtime, caseSpec.Config, caseSpec.Steps, pluginNames, caseSpec.Input,
		) {
			return true
		}
		for j := range caseSpec.Variants {
			variant := &caseSpec.Variants[j]
			if caseExercisesTargetPlugin(
				variant.Runtime, variant.Config, variant.Steps, pluginNames, variant.Input,
			) {
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
	inputs ...HTTPInput,
) bool {
	requestInputs := append([]HTTPInput(nil), inputs...)
	for i := range steps {
		requestInputs = append(requestInputs, steps[i].Input)
	}
	if len(inputs) == 0 {
		return scenarioExercisesTargetPlugin(runtime, config, pluginNames)
	}
	if scenarioExercisesTargetPluginForInputs(runtime, config, pluginNames, requestInputs) {
		return true
	}
	for i := range steps {
		if scenarioExercisesTargetPluginForInputs(
			nil, steps[i].Config, pluginNames, []HTTPInput{steps[i].Input},
		) {
			return true
		}
	}
	return false
}

func scenarioExercisesTargetPluginForInputs(
	runtime, config map[string]any,
	pluginNames []string,
	inputs []HTTPInput,
) bool {
	for _, candidate := range pluginNames {
		if runtimeEnablesPlugin(runtime, candidate) && hasRequestInput(inputs) {
			return true
		}
		if configRoutesRequestThroughPlugin(config, inputs, candidate) {
			return true
		}
	}
	return false
}

func runtimeEnablesPlugin(runtime map[string]any, pluginName string) bool {
	switch plugins := runtime["plugins"].(type) {
	case []any:
		for _, configured := range plugins {
			if configured == pluginName {
				return true
			}
		}
	case []string:
		return slices.Contains(plugins, pluginName)
	}
	return false
}

func hasRequestInput(inputs []HTTPInput) bool {
	for _, input := range inputs {
		if strings.TrimSpace(input.Path) != "" {
			return true
		}
	}
	return false
}

func configRoutesRequestThroughPlugin(config map[string]any, inputs []HTTPInput, pluginName string) bool {
	if !hasRequestInput(inputs) {
		return false
	}
	for _, root := range []string{"global_rules", "global-rules"} {
		if configContainsPlugin(config[root], pluginName) {
			return true
		}
	}
	services := indexedStandaloneResources(config["services"])
	pluginConfigs := indexedStandaloneResources(config["plugin_configs"])
	consumerBound := configContainsPlugin(config["consumers"], pluginName) ||
		configContainsPlugin(config["consumer_groups"], pluginName)
	routes, _ := config["routes"].([]any)
	for _, routeValue := range routes {
		route, ok := routeValue.(map[string]any)
		if !ok || !routeMatchesAnyInput(route, inputs) {
			continue
		}
		if pluginMapActivates(route["plugins"], pluginName) {
			return true
		}
		if service := services[stringValue(route["service_id"])]; pluginMapActivates(service["plugins"], pluginName) {
			return true
		}
		if pluginConfig := pluginConfigs[stringValue(route["plugin_config_id"])]; pluginMapActivates(
			pluginConfig["plugins"],
			pluginName,
		) {
			return true
		}
		if consumerBound && pluginMapContainsAuthentication(route["plugins"]) {
			return true
		}
	}
	return false
}

func pluginMapContainsAuthentication(value any) bool {
	plugins, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, name := range []string{
		"basic-auth", "hmac-auth", "jwt-auth", "key-auth", "ldap-auth",
		"multi-auth", "openid-connect", "wolf-rbac",
	} {
		if _, configured := plugins[name]; configured {
			return true
		}
	}
	return false
}

func pluginMapActivates(value any, pluginName string) bool {
	plugins, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if _, configured := plugins[pluginName]; configured {
		return true
	}
	return configContainsPlugin(plugins, pluginName)
}

func indexedStandaloneResources(value any) map[string]map[string]any {
	indexed := make(map[string]map[string]any)
	resources, _ := value.([]any)
	for _, resourceValue := range resources {
		resource, ok := resourceValue.(map[string]any)
		if !ok {
			continue
		}
		if id := stringValue(resource["id"]); id != "" {
			indexed[id] = resource
		}
	}
	return indexed
}

func stringValue(value any) string {
	valueString, _ := value.(string)
	return valueString
}

func routeMatchesAnyInput(route map[string]any, inputs []HTTPInput) bool {
	for _, input := range inputs {
		requestPath := input.Path
		if before, _, found := strings.Cut(requestPath, "?"); found {
			requestPath = before
		}
		if routeMatchesPath(route, requestPath) {
			return true
		}
	}
	return false
}

func routeMatchesPath(route map[string]any, requestPath string) bool {
	if uri, ok := route["uri"].(string); ok && manifestURIMatches(uri, requestPath) {
		return true
	}
	switch uris := route["uris"].(type) {
	case []any:
		for _, value := range uris {
			if uri, ok := value.(string); ok && manifestURIMatches(uri, requestPath) {
				return true
			}
		}
	case []string:
		for _, uri := range uris {
			if manifestURIMatches(uri, requestPath) {
				return true
			}
		}
	}
	return false
}

func manifestURIMatches(uri, requestPath string) bool {
	candidates := []string{requestPath}
	if decoded, err := url.PathUnescape(requestPath); err == nil && decoded != requestPath {
		candidates = append(candidates, decoded)
	}
	for _, candidate := range slices.Clone(candidates) {
		cleaned := pathpkg.Clean(candidate)
		if cleaned != candidate {
			candidates = append(candidates, cleaned)
		}
	}
	for _, candidate := range candidates {
		if manifestURIPatternMatches(uri, candidate) {
			return true
		}
	}
	return false
}

func manifestURIPatternMatches(uri, requestPath string) bool {
	if uri == requestPath || uri == "/*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(uri, "*"); ok {
		if strings.HasPrefix(requestPath, prefix) {
			return true
		}
	}
	matched, err := pathpkg.Match(uri, requestPath)
	return err == nil && matched
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
