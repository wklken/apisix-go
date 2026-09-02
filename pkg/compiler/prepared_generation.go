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
	publication        generation.PublicationSet
	preparation        PreparationGeneration
	metadata           runtime.MetadataView
	consumers          *runtime.ConsumerBindings
	lookup             consumerLookupView
	tasks              *runtime.TaskRegistry
	effective          *config.EffectiveConfig
	catalog            *capability.SecretDeclarationCatalog
	registry           *runtime.ResourceRegistry
	observers          WorkerRuntimeObservers
	clusterObservers   *clusterObserverRegistry
	materializer       secret.Materializer
	cleanup            *cleanupStack
	detach             func()
	bindingOps         effectiveBindingOps
	trustedClientCAPEM []byte
	httpSnapshot       *HTTPSnapshot
	streamSnapshot     *StreamSnapshot

	materializeMu    sync.Mutex
	bindingOpsMu     sync.Mutex
	closeStartedOnce sync.Once
	terminal         bool
	closeMu          sync.Mutex
	closeAttempt     *preparedCloseAttempt
	cleanupTerminal  bool
	closeErr         error
}

type preparedCloseAttempt struct {
	done chan struct{}
	err  error
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

// HTTP returns the detached, authority-free HTTP/TLS snapshot prepared for
// this generation. A stream-only or closed generation returns nil.
func (prepared *PreparedGeneration) HTTP() *HTTPSnapshot {
	if prepared == nil {
		return nil
	}
	prepared.materializeMu.Lock()
	defer prepared.materializeMu.Unlock()
	if prepared.terminal {
		return nil
	}
	return prepared.httpSnapshot
}

func (prepared *PreparedGeneration) attachHTTP(snapshot *HTTPSnapshot) error {
	if prepared == nil || snapshot == nil || snapshot.Revision() == 0 || snapshot.Handler() == nil {
		return ErrInvalidInput
	}
	prepared.materializeMu.Lock()
	defer prepared.materializeMu.Unlock()
	if prepared.terminal || prepared.httpSnapshot != nil {
		return ErrInvalidInput
	}
	prepared.httpSnapshot = snapshot
	return nil
}

// Stream returns the detached, authority-free stream snapshot prepared for
// this generation. An HTTP-only or closed generation returns nil.
func (prepared *PreparedGeneration) Stream() *StreamSnapshot {
	if prepared == nil {
		return nil
	}
	prepared.materializeMu.Lock()
	defer prepared.materializeMu.Unlock()
	if prepared.terminal {
		return nil
	}
	return prepared.streamSnapshot
}

func (prepared *PreparedGeneration) attachStream(snapshot *StreamSnapshot) error {
	if prepared == nil || snapshot == nil || snapshot.artifact.Domain != generation.DomainStream ||
		snapshot.Revision() == 0 || snapshot.Router() == nil {
		return ErrInvalidInput
	}
	prepared.materializeMu.Lock()
	defer prepared.materializeMu.Unlock()
	if prepared.terminal || prepared.streamSnapshot != nil {
		return ErrInvalidInput
	}
	prepared.streamSnapshot = snapshot
	return nil
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

	prepared.closeMu.Lock()
	if prepared.cleanupTerminal {
		closeErr := prepared.closeErr
		prepared.closeMu.Unlock()
		return closeErr
	}
	if prepared.closeAttempt != nil {
		attempt := prepared.closeAttempt
		prepared.closeMu.Unlock()
		return waitPreparedCloseAttempt(ctx, attempt)
	}
	attempt := &preparedCloseAttempt{done: make(chan struct{})}
	prepared.closeAttempt = attempt
	prepared.materializeMu.Lock()
	cleanup := prepared.cleanup
	prepared.materializeMu.Unlock()
	prepared.closeMu.Unlock()

	var cleanupErr error
	if cleanup != nil {
		cleanupErr = cleanup.Close(ctx)
	}
	terminalCleanup := cleanup == nil || cleanup.terminallyClosed()
	attemptErr := preparedCleanupResult(cleanupErr, terminalCleanup)
	var detach func()
	if terminalCleanup {
		detach = prepared.clearTerminalAuthorities()
	}
	if detach != nil {
		detach()
	}

	prepared.closeMu.Lock()
	if terminalCleanup {
		prepared.cleanupTerminal = true
		prepared.closeErr = attemptErr
	}
	attempt.err = attemptErr
	if prepared.closeAttempt == attempt {
		prepared.closeAttempt = nil
	}
	close(attempt.done)
	prepared.closeMu.Unlock()
	return attemptErr
}

func waitPreparedCloseAttempt(ctx context.Context, attempt *preparedCloseAttempt) error {
	select {
	case <-attempt.done:
		return attempt.err
	default:
	}
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		select {
		case <-attempt.done:
			return attempt.err
		default:
			return ctx.Err()
		}
	}
}

func (prepared *PreparedGeneration) clearTerminalAuthorities() func() {
	prepared.materializeMu.Lock()
	if prepared.httpSnapshot != nil {
		prepared.httpSnapshot.revoke()
	}
	prepared.httpSnapshot = nil
	if prepared.streamSnapshot != nil {
		prepared.streamSnapshot.revoke()
	}
	prepared.streamSnapshot = nil
	prepared.preparation = PreparationGeneration{}
	prepared.metadata = runtime.MetadataView{}
	prepared.consumers = nil
	prepared.lookup = consumerLookupView{}
	prepared.tasks = nil
	prepared.effective = nil
	prepared.catalog = nil
	prepared.registry = nil
	prepared.observers = WorkerRuntimeObservers{}
	prepared.clusterObservers = nil
	prepared.materializer = nil
	prepared.trustedClientCAPEM = nil
	prepared.cleanup = nil
	prepared.bindingOpsMu.Lock()
	prepared.bindingOps = effectiveBindingOps{}
	prepared.bindingOpsMu.Unlock()
	detach := prepared.detach
	prepared.detach = nil
	prepared.materializeMu.Unlock()
	return detach
}

func preparedCleanupResult(cleanupErr error, terminal bool) error {
	if cleanupErr == nil {
		return nil
	}
	if terminal {
		return errPreparedGenerationCleanupFailed
	}
	causes := []error{
		errPreparedGenerationCleanupFailed,
		ErrPreparedGenerationCleanupIncomplete,
	}
	var residual *runtime.TaskResidualError
	if errors.As(cleanupErr, &residual) {
		causes = append(causes, residual)
	}
	for _, marker := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		errWorkerGenerationCleanupFailed,
		errWorkerSecretCleanupFailed,
	} {
		if errors.Is(cleanupErr, marker) {
			causes = append(causes, marker)
		}
	}
	return errors.Join(causes...)
}

type consumerLookupView struct {
	bindings    *runtime.ConsumerBindings
	preparation PreparationGeneration
	catalog     *capability.SecretDeclarationCatalog
	candidates  map[string][]consumerCredentialCandidate
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
