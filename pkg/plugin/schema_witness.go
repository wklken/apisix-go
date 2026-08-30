package plugin

import (
	"fmt"

	"github.com/wklken/apisix-go/pkg/consumer"
)

// SchemaWitness exposes only the immutable schema facts owned by one generated
// plugin factory. Consumer validation remains owned by the consumer package.
type SchemaWitness struct {
	Factory     string
	Config      string
	Metadata    string
	HasConsumer bool
}

// SchemaWitnessForFactory initializes the registered factory only far enough to
// expose its schemas. It deliberately bypasses New so runtime dependencies are
// never installed and no post-initialization lifecycle can run.
func SchemaWitnessForFactory(factory string) (SchemaWitness, error) {
	registered, ok := pluginRegistry[factory]
	if !ok {
		return SchemaWitness{}, fmt.Errorf("schema witness: factory %q is not registered", factory)
	}
	return schemaWitnessFromFactory(factory, registered.create)
}

func schemaWitnessFromFactory(factory string, create func() Plugin) (SchemaWitness, error) {
	if create == nil {
		return SchemaWitness{}, fmt.Errorf("schema witness: factory %q is nil", factory)
	}
	instance := create()
	if instance == nil {
		return SchemaWitness{}, fmt.Errorf("schema witness: factory %q returned nil", factory)
	}
	if err := instance.Init(); err != nil {
		return SchemaWitness{}, fmt.Errorf("schema witness: initialize factory %q: %w", factory, err)
	}
	return SchemaWitness{
		Factory:     factory,
		Config:      instance.GetSchema(),
		Metadata:    instance.GetMetadataSchema(),
		HasConsumer: consumer.Supports(factory),
	}, nil
}
