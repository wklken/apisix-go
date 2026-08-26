package consumer_test

import (
	"maps"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/consumer"
)

func TestRegistryFactoriesMatchManifestConsumerDeclarations(t *testing.T) {
	registryFactories := make(map[string]struct{})
	for _, factory := range consumer.Factories() {
		registryFactories[factory] = struct{}{}
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestFactories := make(map[string]struct{})
	for _, declaration := range catalog.Declarations() {
		if declaration.Source == capability.SecretConsumerConfig {
			manifestFactories[declaration.Factory] = struct{}{}
		}
	}
	if !maps.Equal(registryFactories, manifestFactories) {
		t.Fatalf(
			"resolved registry factories = %v, manifest consumer factories = %v",
			registryFactories,
			manifestFactories,
		)
	}
}
