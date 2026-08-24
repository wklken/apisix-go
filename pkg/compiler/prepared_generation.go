package compiler

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errPreparedGenerationCleanupFailed = errors.New("prepared generation cleanup failed")

// PreparedGeneration owns one fully prepared candidate or recovered
// generation until it is discarded or transferred to a runtime owner.
type PreparedGeneration struct {
	publication  generation.PublicationSet
	attempt      PreparationAttempt
	metadata     runtime.MetadataView
	consumers    *runtime.ConsumerBindings
	lookup       consumerLookupView
	tasks        *runtime.TaskRegistry
	effective    *config.EffectiveConfig
	manifest     *capability.Manifest
	registry     *runtime.ResourceRegistry
	materializer secret.Materializer
	cleanup      *cleanupStack
	detach       func()
	bindingOps   effectiveBindingOps

	materializeMu    sync.Mutex
	bindingOpsMu     sync.Mutex
	closeStartedOnce sync.Once
	terminal         bool
	closeOnce        sync.Once
	closeErr         error
}

// PublicationSet returns a defensive copy of this generation's publication
// identity. A closed generation exposes no candidate data.
func (prepared *PreparedGeneration) PublicationSet() generation.PublicationSet {
	if prepared == nil {
		return generation.PublicationSet{}
	}
	prepared.materializeMu.Lock()
	defer prepared.materializeMu.Unlock()
	if prepared.terminal {
		return generation.PublicationSet{}
	}
	return clonePublicationSetForPreparation(prepared.publication)
}

// MetadataView returns the generation-local immutable metadata view. A closed
// generation returns the zero view.
func (prepared *PreparedGeneration) MetadataView() runtime.MetadataView {
	if prepared == nil {
		return runtime.MetadataView{}
	}
	prepared.materializeMu.Lock()
	defer prepared.materializeMu.Unlock()
	if prepared.terminal {
		return runtime.MetadataView{}
	}
	return prepared.metadata
}

// ConsumerLookup returns a decode-only view without consumer cleanup
// authority. A closed generation returns no lookup.
func (prepared *PreparedGeneration) ConsumerLookup() base.ConsumerLookup {
	if prepared == nil {
		return nil
	}
	prepared.materializeMu.Lock()
	defer prepared.materializeMu.Unlock()
	if prepared.terminal || prepared.lookup.bindings == nil {
		return nil
	}
	return prepared.lookup
}

// DiscardPrepared releases this generation only when set is its complete
// publication identity.
func (prepared *PreparedGeneration) DiscardPrepared(
	ctx context.Context,
	set generation.PublicationSet,
) error {
	if prepared == nil {
		return ErrPreparedSetMismatch
	}
	prepared.materializeMu.Lock()
	matches := reflect.DeepEqual(prepared.publication, set)
	prepared.materializeMu.Unlock()
	if !matches {
		return ErrPreparedSetMismatch
	}
	return prepared.Close(ctx)
}

// Close releases every generation-owned resource exactly once and detaches
// the generation from its factory owner.
func (prepared *PreparedGeneration) Close(ctx context.Context) error {
	if prepared == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx := context.WithoutCancel(ctx)
	prepared.bindingOpsMu.Lock()
	closeStarted := prepared.bindingOps.closeStarted
	prepared.bindingOpsMu.Unlock()
	prepared.closeStartedOnce.Do(func() {
		if closeStarted != nil {
			closeStarted()
		}
	})

	prepared.materializeMu.Lock()
	prepared.terminal = true
	prepared.materializeMu.Unlock()

	prepared.closeOnce.Do(func() {
		prepared.materializeMu.Lock()
		cleanup := prepared.cleanup
		prepared.materializeMu.Unlock()
		if cleanup != nil {
			if cleanup.Close(cleanupCtx) != nil {
				prepared.closeErr = errPreparedGenerationCleanupFailed
			}
		}

		prepared.materializeMu.Lock()
		prepared.attempt = PreparationAttempt{}
		prepared.metadata = runtime.MetadataView{}
		prepared.consumers = nil
		prepared.lookup = consumerLookupView{}
		prepared.tasks = nil
		prepared.effective = nil
		prepared.manifest = nil
		prepared.registry = nil
		prepared.materializer = nil
		prepared.cleanup = nil
		prepared.bindingOpsMu.Lock()
		prepared.bindingOps = effectiveBindingOps{}
		prepared.bindingOpsMu.Unlock()
		detach := prepared.detach
		prepared.detach = nil
		prepared.materializeMu.Unlock()
		if detach != nil {
			detach()
		}
	})
	return prepared.closeErr
}

type consumerLookupView struct {
	bindings *runtime.ConsumerBindings
}

func (view consumerLookupView) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	if view.bindings == nil {
		return resource.Consumer{}, false
	}
	return view.bindings.ConsumerByPluginKey(plugin, key)
}

func (view consumerLookupView) ConsumerByID(id string) (resource.Consumer, bool) {
	if view.bindings == nil {
		return resource.Consumer{}, false
	}
	return view.bindings.ConsumerByID(id)
}

func (view consumerLookupView) ConsumerGroupByID(id string) (resource.ConsumerGroup, bool) {
	if view.bindings == nil {
		return resource.ConsumerGroup{}, false
	}
	return view.bindings.ConsumerGroupByID(id)
}
