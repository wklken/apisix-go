package plugin

import (
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestCapabilityManifestWindowsPlatformsCrossBuild(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}

	packageSet := make(map[string]struct{})
	for _, plugin := range manifest.Plugins {
		if !slices.Contains(plugin.SupportedPlatforms, "windows-amd64") {
			continue
		}
		for _, factory := range plugin.Factories {
			packageSet[factory.ImportPath] = struct{}{}
		}
	}

	packages := make([]string, 0, len(packageSet))
	for importPath := range packageSet {
		packages = append(packages, importPath)
	}
	sort.Strings(packages)
	if len(packages) == 0 {
		t.Fatal("capability manifest has no factory packages claiming windows-amd64 support")
	}

	args := append([]string{"build"}, packages...)
	cmd := exec.Command("go", args...)
	cmd.Env = slices.DeleteFunc(append([]string(nil), os.Environ()...), func(value string) bool {
		name, _, _ := strings.Cut(value, "=")
		return strings.EqualFold(name, "GOOS") || strings.EqualFold(name, "GOARCH")
	})
	cmd.Env = append(cmd.Env, "GOOS=windows", "GOARCH=amd64")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-build windows-amd64 factory packages %v: %v\n%s", packages, err, output)
	}
}

func TestCapabilityManifestCoversEveryFactory(t *testing.T) {
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	manifestFactories := make(map[string]struct{})
	for _, entry := range m.Plugins {
		for _, factory := range entry.Factories {
			manifestFactories[factory.Key] = struct{}{}
		}
	}
	var missing, extra []string
	for key := range pluginRegistry {
		if _, ok := manifestFactories[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range manifestFactories {
		if _, ok := pluginRegistry[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("factory inventory mismatch: missing=%v extra=%v", missing, extra)
	}
}

func TestCapabilityManifestMatchesRuntimeFacts(t *testing.T) {
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	for key, factory := range pluginRegistry {
		entry, ok := m.Plugin(key)
		if !ok {
			t.Fatalf("factory %s is absent from the capability manifest", key)
		}
		if factory == nil {
			t.Fatalf("factory %s is nil", key)
		}
		instance := factory()
		if instance == nil {
			t.Fatalf("factory %s returned nil", key)
		}
		if err := instance.Init(); err != nil {
			t.Fatalf("%s Init: %v", key, err)
		}
		if instance.GetName() != entry.Implementation {
			t.Fatalf(
				"%s implementation = %q, manifest = %q",
				key,
				instance.GetName(),
				entry.Implementation,
			)
		}
		if instance.GetPriority() != entry.Priority {
			t.Fatalf(
				"%s priority = %d, manifest = %d",
				key,
				instance.GetPriority(),
				entry.Priority,
			)
		}

		descriptor, err := DescriptorForFactory(m, key)
		if err != nil {
			t.Fatalf("factory %s descriptor: %v", key, err)
		}
		wantPhases := make([]string, len(descriptor.Phases))
		for index, phase := range descriptor.Phases {
			wantPhases[index] = string(phase)
		}
		if !slices.Equal(entry.Phases, wantPhases) {
			t.Fatalf("%s phases = %v, descriptor = %v", key, entry.Phases, wantPhases)
		}
	}
}
