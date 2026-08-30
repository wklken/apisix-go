package plugin

import (
	"slices"
	"testing"
)

func TestDefinitionsCoverEveryFactoryInStableOrder(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != len(pluginRegistry) {
		t.Fatalf("definitions = %d, registry = %d", len(definitions), len(pluginRegistry))
	}
	factories := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		if _, ok := pluginRegistry[definition.Factory]; !ok {
			t.Fatalf("definition %q has no constructor", definition.Factory)
		}
		factories = append(factories, definition.Factory)
	}
	if !slices.IsSorted(factories) {
		t.Fatalf("definitions are not sorted: %v", factories)
	}
}

func TestRegistryMatchesPluginOwnedRuntimeFacts(t *testing.T) {
	for key, registered := range pluginRegistry {
		if registered.create == nil {
			t.Fatalf("factory %s is nil", key)
		}
		instance := registered.create()
		if instance == nil {
			t.Fatalf("factory %s returned nil", key)
		}
		if err := instance.Init(); err != nil {
			t.Fatalf("%s Init: %v", key, err)
		}
		wantName := key
		if alias, ok := pluginNameAliases[key]; ok {
			wantName = alias
		}
		if instance.GetName() != wantName {
			t.Fatalf("%s implementation name = %q, want %q", key, instance.GetName(), wantName)
		}

		descriptor, err := DescriptorForFactory(key)
		if err != nil {
			t.Fatalf("factory %s descriptor: %v", key, err)
		}
		if descriptor.Factory != key {
			t.Fatalf("factory %s descriptor identity = %q", key, descriptor.Factory)
		}
	}
}

func TestDefinitionReturnsDefensiveCopies(t *testing.T) {
	left, ok := DefinitionForFactory("request-id")
	if !ok {
		t.Fatal("request-id definition is missing")
	}
	left.Phases[0] = PhaseLog
	left.Scopes[0] = ScopeSystem
	right, _ := DefinitionForFactory("request-id")
	if right.Phases[0] != PhaseRewrite || right.Scopes[0] != ScopeGlobal {
		t.Fatalf("registry definition was mutated: %#v", right)
	}
}
