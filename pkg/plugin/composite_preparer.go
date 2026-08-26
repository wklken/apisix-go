package plugin

import (
	"context"
	"errors"
	"sync"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

var (
	errCompositeChildInputs  = errors.New("composite child preparer: invalid immutable inputs")
	errCompositeChildSpec    = errors.New("composite child preparer: invalid factory, position, or config")
	errCompositeChildPrepare = errors.New("composite child preparer: child preparation failed")
)

type compositeChildPreparer struct {
	dependencies base.Dependencies
	attempt      secret.AttemptID
	scope        Scope
	provenance   ResourceProvenance
}

type preparedCompositeChild struct {
	binding Binding
	close   sync.Once
}

func NewCompositeChildPreparer(
	dependencies base.Dependencies,
	attempt secret.AttemptID,
	scope Scope,
	provenance ResourceProvenance,
) (base.CompositeChildPreparer, error) {
	if attempt == (secret.AttemptID{}) || !dependencies.Secrets.Valid() ||
		dependencies.Secrets.AttemptID() != attempt ||
		!validCompositeOuterIdentity(scope, provenance) {
		return nil, errCompositeChildInputs
	}
	return &compositeChildPreparer{
		dependencies: dependencies,
		attempt:      attempt,
		scope:        scope,
		provenance:   provenance,
	}, nil
}

func (preparer *compositeChildPreparer) Prepare(
	ctx context.Context,
	access base.ScopedSecretAccess,
	spec base.CompositeChildSpec,
) (base.PreparedCompositeChild, error) {
	if ctx == nil {
		return nil, errCompositeChildInputs
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if preparer == nil || spec.Factory == "" || spec.Position == "" || spec.Config == nil {
		return nil, errCompositeChildSpec
	}
	if !access.ValidFor(preparer.dependencies.Secrets) {
		return nil, errCompositeChildInputs
	}

	childAccess, err := access.Child(spec.Factory)
	if err != nil {
		return nil, compositeChildError(err)
	}
	clonedConfig := cloneCompositeConfig(spec.Config)
	childDependencies := preparer.dependencies
	childDependencies.CompositeChildren = nil
	factoryInstance, err := newCompositeFactoryInstance(spec.Factory, childDependencies)
	if err != nil {
		return nil, compositeChildError(err)
	}
	child := factoryInstance.Plugin()
	if isNilPlugin(child) {
		return nil, errCompositeChildPrepare
	}

	var stopOnce sync.Once
	stopChild := func() {
		stopOnce.Do(func() {
			if stopper, ok := child.(interface{ Stop() }); ok {
				stopper.Stop()
			}
		})
	}
	fail := func(err error) (base.PreparedCompositeChild, error) {
		stopChild()
		return nil, compositeChildError(err)
	}

	if err := child.Init(); err != nil {
		return fail(err)
	}
	compiledSchema, err := util.CompileSchema(child.GetSchema())
	if err != nil {
		return fail(err)
	}
	if err := compiledSchema.Validate(clonedConfig); err != nil {
		return fail(err)
	}
	childConfig := child.Config()
	if childConfig == nil {
		return fail(errCompositeChildPrepare)
	}
	if err := util.Parse(clonedConfig, childConfig); err != nil {
		return fail(err)
	}
	if err := base.MaterializeScopedCompositeChildSecrets(ctx, childAccess, child); err != nil {
		return fail(err)
	}
	if err := child.PostInit(); err != nil {
		return fail(err)
	}
	descriptor, err := ResolveDescriptorForFactory(factoryInstance.Factory(), child)
	if err != nil {
		return fail(err)
	}
	binding, err := BindAttemptResolvedPlugin(
		preparer.attempt,
		descriptor,
		child,
		preparer.scope,
		preparer.provenance,
		InstanceIdentityInput{
			PluginConfig:      child.Config(),
			CompositePosition: spec.Position,
		},
	)
	if err != nil {
		return fail(err)
	}
	return &preparedCompositeChild{binding: binding}, nil
}

func newCompositeFactoryInstance(
	factory string,
	dependencies base.Dependencies,
) (instance FactoryInstance, err error) {
	defer func() {
		if recover() != nil {
			instance = FactoryInstance{}
			err = errCompositeChildPrepare
		}
	}()
	return NewFactoryInstance(factory, dependencies)
}

func (child *preparedCompositeChild) Factory() string {
	if child == nil {
		return ""
	}
	return child.binding.Descriptor.Factory
}

func (child *preparedCompositeChild) Instance() any {
	if child == nil {
		return nil
	}
	return child.binding.Plugin
}

func (child *preparedCompositeChild) Close() {
	if child == nil {
		return
	}
	child.close.Do(func() {
		if stopper, ok := child.binding.Plugin.(interface{ Stop() }); ok {
			stopper.Stop()
		}
	})
}

func compositeChildError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errCompositeChildPrepare
}

func validCompositeOuterIdentity(scope Scope, provenance ResourceProvenance) bool {
	if provenance.Kind == "" || provenance.ID == "" {
		return false
	}
	switch scope {
	case ScopeSystem:
		return provenance.Kind == ResourceSystem
	case ScopeGlobal:
		return provenance.Kind == ResourceGlobalRule
	case ScopeRoute:
		return provenance.Kind == ResourceRoute || provenance.Kind == ResourceService ||
			provenance.Kind == ResourcePluginConfig
	case ScopeConsumer:
		return provenance.Kind == ResourceConsumer || provenance.Kind == ResourceConsumerGroup
	default:
		return false
	}
}

func cloneCompositeConfig(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneCompositeValue(value)
	}
	return cloned
}

func cloneCompositeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneCompositeConfig(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneCompositeValue(item)
		}
		return cloned
	default:
		return value
	}
}
