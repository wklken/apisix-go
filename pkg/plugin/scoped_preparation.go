package plugin

import (
	"errors"
	"reflect"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

var (
	errScopedSecretFactoryRequired    = errors.New("plugin scoped secret materialization: factory is required")
	errScopedSecretFactoryUnavailable = errors.New("plugin scoped secret materialization: factory unavailable")
)

// FactoryInstance binds a plugin instance to the exact registry key that
// constructed it. Its fields are intentionally private so callers cannot
// relabel one registered factory as another factory with the same Go type.
type FactoryInstance struct {
	factory  string
	instance Plugin
}

// NewFactoryInstance constructs a plugin while retaining its exact registry
// identity. Plugin lifecycle methods are not invoked.
func NewFactoryInstance(factory string, deps base.Dependencies) (FactoryInstance, error) {
	if factory == "" {
		return FactoryInstance{}, errScopedSecretFactoryRequired
	}
	instance := New(factory, deps)
	if isNilPlugin(instance) {
		return FactoryInstance{}, errScopedSecretFactoryUnavailable
	}
	return FactoryInstance{factory: factory, instance: instance}, nil
}

func (instance FactoryInstance) Factory() string { return instance.factory }

func (instance FactoryInstance) Plugin() Plugin { return instance.instance }

// SupportsScopedSecretMaterialization reports whether the registered factory
// implements the generation-scoped secret contract. The factory is constructed
// only for a type assertion; no plugin lifecycle or materialization method is
// invoked.
func SupportsScopedSecretMaterialization(factory string) (bool, error) {
	if factory == "" {
		return false, errScopedSecretFactoryRequired
	}
	registered, ok := pluginRegistry[factory]
	if !ok {
		return false, errScopedSecretFactoryUnavailable
	}
	return scopedSecretMaterializationSupportFromFactory(factory, registered.create)
}

func scopedSecretMaterializationSupportFromFactory(factory string, create func() Plugin) (bool, error) {
	if factory == "" {
		return false, errScopedSecretFactoryRequired
	}
	if create == nil {
		return false, errScopedSecretFactoryUnavailable
	}
	instance := create()
	if isNilPlugin(instance) {
		return false, errScopedSecretFactoryUnavailable
	}
	_, supported := instance.(base.ScopedSecretMaterializer)
	return supported, nil
}

func isNilPlugin(instance Plugin) bool {
	if instance == nil {
		return true
	}
	value := reflect.ValueOf(instance)
	return value.Kind() == reflect.Pointer && value.IsNil()
}
