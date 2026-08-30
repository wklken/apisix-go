package consumer_test

import (
	"maps"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/consumer"
)

func TestRegistryFactoriesMatchConsumerSecretDeclarations(t *testing.T) {
	registryFactories := make(map[string]struct{})
	for _, factory := range consumer.Factories() {
		registryFactories[factory] = struct{}{}
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	declaredFactories := make(map[string]struct{})
	for _, declaration := range catalog.Declarations() {
		if declaration.Source == capability.SecretConsumerConfig {
			declaredFactories[declaration.Factory] = struct{}{}
		}
	}
	if !maps.Equal(registryFactories, declaredFactories) {
		t.Fatalf(
			"resolved registry factories = %v, consumer declaration factories = %v",
			registryFactories,
			declaredFactories,
		)
	}
}
