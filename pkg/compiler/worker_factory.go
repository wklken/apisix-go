package compiler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/tlsconfig"
)

var (
	errWorkerGenerationPreparationFailed  = errors.New("worker generation preparation failed")
	errWorkerGenerationCleanupFailed      = errors.New("worker generation cleanup failed")
	errWorkerRegistrationCleanupFailed    = errors.New("worker attempt registration cleanup failed")
	errWorkerCompilerFactoryCleanupFailed = errors.New("worker compiler factory cleanup failed")
)

type workerFactoryCheckpointState struct {
	tasks     *runtime.TaskRegistry
	consumers *runtime.ConsumerBindings
}

// WorkerCompilerFactory owns the immutable compiler inputs and shared runtime
// registry used to prepare candidate generations.
type WorkerCompilerFactory struct {
	compiler           *Compiler
	effective          *config.EffectiveConfig
	materializer       secret.Materializer
	attempts           *attemptFactory
	consumers          ConsumerPreparer
	metadata           MetadataPreparer
	registry           *runtime.ResourceRegistry
	observers          WorkerRuntimeObservers
	clusterObservers   *clusterObserverRegistry
	bindingOps         effectiveBindingOps
	trustedClientCAPEM []byte

	gate   sync.RWMutex
	closed bool

	liveMu sync.Mutex
	live   map[secret.AttemptID]*PreparedGeneration

	closeMu       sync.Mutex
	closeAttempt  *workerFactoryCloseAttempt
	closeTerminal bool
	closeErrors   []error
	closeErr      error

	checkpoint func(string, workerFactoryCheckpointState) error
}

type workerFactoryCloseAttempt struct {
	done chan struct{}
	err  error
}

// NewWorkerCompilerFactory constructs the candidate-generation compiler and
// validates every immutable authority before allocating lifecycle owners.
func NewWorkerCompilerFactory(
	manifest *capability.Manifest,
	effective *config.EffectiveConfig,
	materializer secret.Materializer,
	observers WorkerRuntimeObservers,
) (*WorkerCompilerFactory, error) {
	if manifest == nil || effective == nil || isNilInterface(materializer) {
		return nil, fmt.Errorf("%w: worker compiler dependencies are required", ErrInvalidInput)
	}
	if err := validateWorkerRuntimeObservers(effective, observers); err != nil {
		return nil, err
	}
	clusterObservers, err := newClusterObserverRegistry(observers.Cluster)
	if err != nil {
		return nil, err
	}
	compiler, err := New(manifest)
	if err != nil {
		return nil, err
	}
	ownedEffective, err := cloneWorkerEffectiveConfig(effective)
	if err != nil {
		return nil, fmt.Errorf("%w: effective config is not defensively ownable", ErrInvalidInput)
	}
	if ownedEffective.Config.Profiles() != ownedEffective.Profiles {
		return nil, fmt.Errorf("%w: effective config profile selection is inconsistent", ErrInvalidInput)
	}
	if err := ownedEffective.Profiles.Validate(compiler.manifest); err != nil {
		return nil, fmt.Errorf("%w: effective config profile selection is invalid", ErrInvalidInput)
	}
	trustedClientCAPEM, err := readWorkerTrustedClientCA(&ownedEffective.Config)
	if err != nil {
		return nil, err
	}
	attempts, err := newAttemptFactory(compiler, materializer)
	if err != nil {
		return nil, err
	}
	consumers, err := newConsumerBindingPreparer(compiler.schemas.catalog)
	if err != nil {
		return nil, err
	}
	metadata, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		return nil, err
	}
	return &WorkerCompilerFactory{
		compiler: compiler, effective: ownedEffective, materializer: materializer,
		attempts: attempts, consumers: consumers, metadata: metadata,
		registry: runtime.NewResourceRegistry(), observers: observers,
		clusterObservers: clusterObservers, bindingOps: defaultEffectiveBindingOps(),
		trustedClientCAPEM: trustedClientCAPEM,
		live:               make(map[secret.AttemptID]*PreparedGeneration),
	}, nil
}

func readWorkerTrustedClientCA(cfg *config.Config) ([]byte, error) {
	if !tlsconfig.FrontendEnabled(cfg) {
		return nil, nil
	}
	path := strings.TrimSpace(cfg.Apisix.Ssl.SslTrustedCertificate)
	if path == "" {
		return nil, nil
	}
	certificatePEM, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trusted client CA %q: %w", path, err)
	}
	return bytes.Clone(certificatePEM), nil
}

// PrepareGeneration compiles, registers, and atomically transfers one complete
// candidate generation. It never returns a partial lifecycle owner.
func (factory *WorkerCompilerFactory) PrepareGeneration(
	ctx context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	previous map[generation.Domain]generation.PublishedGeneration,
	onFailure func(runtime.TaskFailure),
) (*PreparedGeneration, error) {
	if factory == nil || ctx == nil {
		return nil, fmt.Errorf("%w: worker compiler factory and context are required", ErrInvalidInput)
	}
	factory.gate.RLock()
	defer factory.gate.RUnlock()
	if factory.closed {
		return nil, ErrWorkerCompilerFactoryClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, workerGenerationFailure(err, nil)
	}

	registered, err := factory.attempts.prepareCandidateAttempt(ctx, ticket, desired, previous)
	if err != nil {
		return nil, workerGenerationFailure(err, nil)
	}
	return factory.transferRegisteredGeneration(ctx, registered, onFailure)
}

// PrepareRecovery verifies committed publication state, registers its recovery
// attempt, and transfers the same generation owners as candidate preparation.
func (factory *WorkerCompilerFactory) PrepareRecovery(
	ctx context.Context,
	revisions generation.RevisionSet,
	committed map[generation.Domain]generation.PublishedGeneration,
	onFailure func(runtime.TaskFailure),
) (*PreparedGeneration, error) {
	if factory == nil || ctx == nil {
		return nil, fmt.Errorf("%w: worker compiler factory and context are required", ErrInvalidInput)
	}
	factory.gate.RLock()
	defer factory.gate.RUnlock()
	if factory.closed {
		return nil, ErrWorkerCompilerFactoryClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, workerGenerationFailure(err, nil)
	}

	registered, err := factory.attempts.prepareRecoveryAttempt(ctx, revisions, committed)
	if err != nil {
		return nil, workerGenerationFailure(err, nil)
	}
	return factory.transferRegisteredGeneration(ctx, registered, onFailure)
}

// Close terminally stops every live generation before closing the shared
// resource registry. Incomplete attempts retain ownership for a later retry.
func (factory *WorkerCompilerFactory) Close(ctx context.Context) error {
	if factory == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	factory.gate.Lock()
	factory.closed = true
	factory.gate.Unlock()

	factory.closeMu.Lock()
	if factory.closeTerminal {
		closeErr := factory.closeErr
		factory.closeMu.Unlock()
		return closeErr
	}
	if factory.closeAttempt != nil {
		attempt := factory.closeAttempt
		factory.closeMu.Unlock()
		return waitWorkerFactoryCloseAttempt(ctx, attempt)
	}
	attempt := &workerFactoryCloseAttempt{done: make(chan struct{})}
	factory.closeAttempt = attempt
	factory.closeMu.Unlock()

	live := factory.liveGenerations()
	var attemptErrors []error
	for _, generationOwner := range live {
		generationErr := generationOwner.prepared.Close(ctx)
		if generationErr == nil {
			continue
		}
		if workerCleanupIncomplete(generationErr) {
			attemptErrors = append(attemptErrors, generationErr)
			continue
		}
		factory.appendCloseError(generationErr)
	}

	incomplete := len(attemptErrors) != 0 || factory.liveCount() != 0
	if !incomplete && factory.registry != nil {
		registryErr := factory.registry.Close(ctx)
		if registryErr != nil {
			if workerCleanupIncomplete(registryErr) {
				attemptErrors = append(attemptErrors, workerRetryableCleanupResult(registryErr))
				incomplete = true
			} else {
				factory.appendCloseError(errWorkerCompilerFactoryCleanupFailed)
			}
		}
	}

	factory.closeMu.Lock()
	if incomplete {
		attempt.err = factory.joinCloseErrors(attemptErrors)
	} else {
		factory.closeTerminal = true
		attempt.err = factory.joinCloseErrors(nil)
		factory.closeErr = attempt.err
	}
	if factory.closeAttempt == attempt {
		factory.closeAttempt = nil
	}
	close(attempt.done)
	factory.closeMu.Unlock()
	return attempt.err
}

func (factory *WorkerCompilerFactory) liveGenerations() []struct {
	attemptID secret.AttemptID
	prepared  *PreparedGeneration
} {
	factory.liveMu.Lock()
	live := make([]struct {
		attemptID secret.AttemptID
		prepared  *PreparedGeneration
	}, 0, len(factory.live))
	for attemptID, prepared := range factory.live {
		live = append(live, struct {
			attemptID secret.AttemptID
			prepared  *PreparedGeneration
		}{attemptID: attemptID, prepared: prepared})
	}
	factory.liveMu.Unlock()
	slices.SortFunc(live, func(left, right struct {
		attemptID secret.AttemptID
		prepared  *PreparedGeneration
	},
	) int {
		return bytes.Compare(left.attemptID[:], right.attemptID[:])
	})
	return live
}

func (factory *WorkerCompilerFactory) liveCount() int {
	factory.liveMu.Lock()
	defer factory.liveMu.Unlock()
	return len(factory.live)
}

func (factory *WorkerCompilerFactory) appendCloseError(err error) {
	if err == nil {
		return
	}
	factory.closeMu.Lock()
	defer factory.closeMu.Unlock()
	if slices.Contains(factory.closeErrors, err) {
		return
	}
	factory.closeErrors = append(factory.closeErrors, err)
}

func (factory *WorkerCompilerFactory) joinCloseErrors(attemptErrors []error) error {
	joined := make([]error, 0, len(factory.closeErrors)+len(attemptErrors)+1)
	joined = append(joined, factory.closeErrors...)
	joined = append(joined, attemptErrors...)
	if len(joined) == 0 {
		return nil
	}
	return errors.Join(append([]error{errWorkerCompilerFactoryCleanupFailed}, joined...)...)
}

func waitWorkerFactoryCloseAttempt(ctx context.Context, attempt *workerFactoryCloseAttempt) error {
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

func workerCleanupIncomplete(err error) bool {
	var residual *runtime.TaskResidualError
	return errors.As(err, &residual) || errors.Is(err, ErrPreparedGenerationCleanupIncomplete) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func workerRetryableCleanupResult(err error) error {
	var causes []error
	var residual *runtime.TaskResidualError
	if errors.As(err, &residual) {
		causes = append(causes, residual)
	}
	for _, marker := range []error{
		ErrPreparedGenerationCleanupIncomplete,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		if errors.Is(err, marker) {
			causes = append(causes, marker)
		}
	}
	return errors.Join(causes...)
}

// transferRegisteredGeneration is the single candidate/recovery ownership
// transfer. Future recovery preparation supplies only its registered attempt.
func (factory *WorkerCompilerFactory) transferRegisteredGeneration(
	ctx context.Context,
	registered *registeredAttempt,
	onFailure func(runtime.TaskFailure),
) (*PreparedGeneration, error) {
	cleanup := &cleanupStack{}
	var prepared *PreparedGeneration
	fail := func(primary error) (*PreparedGeneration, error) {
		var cleanupErr error
		if prepared != nil {
			cleanupErr = prepared.Close(context.WithoutCancel(ctx))
		} else {
			cleanupErr = cleanup.Close(context.WithoutCancel(ctx))
		}
		return nil, workerGenerationFailure(primary, cleanupErr)
	}

	if registered == nil || registered.attempt.authority == nil ||
		!registered.attempt.capability.Valid() || registered.attempt.Generation() == 0 {
		return fail(ErrInvalidInput)
	}
	if err := cleanup.Own(cleanupRelease, "attempt-registration", func(closeCtx context.Context) error {
		if err := registered.Close(closeCtx); err != nil {
			return errWorkerRegistrationCleanupFailed
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	if err := factory.runCheckpoint("attempt-and-capability-ready", workerFactoryCheckpointState{}); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	tasks := runtime.NewTaskRegistry(context.WithoutCancel(ctx), onFailure)
	if err := cleanup.Own(cleanupQuiesce, "generation-tasks", func(stopCtx context.Context) error {
		residuals, stopErr := tasks.Stop(stopCtx)
		if stopErr != nil || len(residuals) != 0 {
			return errors.Join(errWorkerGenerationCleanupFailed, stopErr)
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	state := workerFactoryCheckpointState{tasks: tasks}
	if err := factory.runCheckpoint("create-task-registry", state); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	bindings, consumerErr := factory.consumers.PrepareConsumers(ctx, registered.attempt)
	if bindings != nil {
		if err := cleanup.Own(cleanupRelease, "consumer-bindings", func(context.Context) error {
			bindings.Close()
			return nil
		}); err != nil {
			bindings.Close()
			return fail(err)
		}
	}
	if consumerErr != nil {
		return fail(consumerErr)
	}
	if bindings == nil {
		return fail(ErrInvalidInput)
	}
	state.consumers = bindings
	if err := factory.runCheckpoint("prepare-consumers", state); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	metadata, err := factory.metadata.PrepareMetadata(ctx, registered.attempt)
	if err != nil {
		return fail(err)
	}
	if err := cleanup.Own(cleanupRelease, "plugin-metadata", func(context.Context) error {
		metadata.Close()
		return nil
	}); err != nil {
		metadata.Close()
		return fail(err)
	}
	if err := factory.runCheckpoint("prepare-metadata", state); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	attemptID := registered.attempt.AttemptID()
	prepared = &PreparedGeneration{
		publication: clonePublicationSetForPreparation(registered.publication),
		attempt:     registered.attempt, metadata: metadata, consumers: bindings,
		lookup: consumerLookupView{bindings: bindings}, tasks: tasks,
		effective: factory.effective, manifest: factory.compiler.manifest,
		registry: factory.registry, materializer: factory.materializer, cleanup: cleanup,
		observers:          factory.observers,
		clusterObservers:   factory.clusterObservers,
		taskFailure:        onFailure,
		bindingOps:         factory.bindingOps.withDefaults(attemptID),
		trustedClientCAPEM: bytes.Clone(factory.trustedClientCAPEM),
	}
	if err := factory.runCheckpoint("bind-private-materializer-authority", state); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := prepared.compileAndAttachHTTP(ctx); err != nil {
		return fail(err)
	}
	if err := factory.runCheckpoint("compile-http-snapshot", state); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := prepared.compileAndAttachStream(ctx); err != nil {
		return fail(err)
	}
	if err := factory.runCheckpoint("compile-stream-snapshot", state); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	prepared.detach = func() {
		factory.liveMu.Lock()
		if factory.live[attemptID] == prepared {
			delete(factory.live, attemptID)
		}
		factory.liveMu.Unlock()
	}
	factory.liveMu.Lock()
	_, collision := factory.live[attemptID]
	if !collision {
		factory.live[attemptID] = prepared
	}
	factory.liveMu.Unlock()
	if collision {
		return fail(ErrInvalidInput)
	}
	if err := factory.runCheckpoint("transfer-prepared-generation", state); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return prepared, nil
}

func (factory *WorkerCompilerFactory) runCheckpoint(
	stage string,
	state workerFactoryCheckpointState,
) error {
	if factory.checkpoint == nil {
		return nil
	}
	return factory.checkpoint(stage, state)
}

func workerGenerationFailure(primary, cleanup error) error {
	causes := []error{errWorkerGenerationPreparationFailed}
	if errors.Is(primary, context.Canceled) {
		causes = append(causes, context.Canceled)
	}
	if errors.Is(primary, context.DeadlineExceeded) {
		causes = append(causes, context.DeadlineExceeded)
	}
	if cleanup != nil {
		causes = append(causes, errWorkerGenerationCleanupFailed)
	}
	return errors.Join(causes...)
}

func cloneWorkerEffectiveConfig(source *config.EffectiveConfig) (*config.EffectiveConfig, error) {
	cloned, err := cloneWorkerEffectiveValue(reflect.ValueOf(*source))
	if err != nil {
		return nil, err
	}
	result := cloned.Interface().(config.EffectiveConfig)
	return &result, nil
}

func cloneWorkerEffectiveValue(source reflect.Value) (reflect.Value, error) {
	return cloneWorkerEffectiveValuePath(source, make(map[workerEffectiveReferenceIdentity]struct{}))
}

type workerEffectiveReferenceIdentity struct {
	kind     reflect.Kind
	typeName reflect.Type
	pointer  uintptr
	length   int
	capacity int
}

func cloneWorkerEffectiveValuePath(
	source reflect.Value,
	path map[workerEffectiveReferenceIdentity]struct{},
) (reflect.Value, error) {
	if !source.IsValid() {
		return reflect.Value{}, nil
	}
	identity, tracked := workerEffectiveIdentity(source)
	if tracked {
		if _, cyclic := path[identity]; cyclic {
			return reflect.Value{}, fmt.Errorf("cyclic effective value %s is unsupported", source.Type())
		}
		path[identity] = struct{}{}
		defer delete(path, identity)
	}
	switch source.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String:
		return source, nil
	case reflect.Interface:
		if source.IsNil() {
			return reflect.Zero(source.Type()), nil
		}
		value, err := cloneWorkerEffectiveValuePath(source.Elem(), path)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(source.Type()).Elem()
		result.Set(value)
		return result, nil
	case reflect.Pointer:
		if source.IsNil() {
			return reflect.Zero(source.Type()), nil
		}
		value, err := cloneWorkerEffectiveValuePath(source.Elem(), path)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(source.Type().Elem())
		result.Elem().Set(value)
		return result, nil
	case reflect.Slice:
		if source.IsNil() {
			return reflect.Zero(source.Type()), nil
		}
		result := reflect.MakeSlice(source.Type(), source.Len(), source.Len())
		for index := range source.Len() {
			value, err := cloneWorkerEffectiveValuePath(source.Index(index), path)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(index).Set(value)
		}
		return result, nil
	case reflect.Array:
		result := reflect.New(source.Type()).Elem()
		for index := range source.Len() {
			value, err := cloneWorkerEffectiveValuePath(source.Index(index), path)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(index).Set(value)
		}
		return result, nil
	case reflect.Map:
		if source.IsNil() {
			return reflect.Zero(source.Type()), nil
		}
		result := reflect.MakeMapWithSize(source.Type(), source.Len())
		iterator := source.MapRange()
		for iterator.Next() {
			key, err := cloneWorkerEffectiveValuePath(iterator.Key(), path)
			if err != nil {
				return reflect.Value{}, err
			}
			value, err := cloneWorkerEffectiveValuePath(iterator.Value(), path)
			if err != nil {
				return reflect.Value{}, err
			}
			result.SetMapIndex(key, value)
		}
		return result, nil
	case reflect.Struct:
		result := reflect.New(source.Type()).Elem()
		for index := range source.NumField() {
			if source.Type().Field(index).PkgPath != "" {
				return reflect.Value{}, fmt.Errorf("opaque struct %s is unsupported", source.Type())
			}
			value, err := cloneWorkerEffectiveValuePath(source.Field(index), path)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Field(index).Set(value)
		}
		return result, nil
	case reflect.Invalid:
		return reflect.Value{}, nil
	default:
		return reflect.Value{}, fmt.Errorf("opaque effective value %s is unsupported", source.Kind())
	}
}

func workerEffectiveIdentity(source reflect.Value) (workerEffectiveReferenceIdentity, bool) {
	switch source.Kind() {
	case reflect.Map, reflect.Pointer:
		if source.IsNil() {
			return workerEffectiveReferenceIdentity{}, false
		}
		return workerEffectiveReferenceIdentity{
			kind: source.Kind(), typeName: source.Type(), pointer: source.Pointer(),
		}, true
	case reflect.Slice:
		if source.IsNil() {
			return workerEffectiveReferenceIdentity{}, false
		}
		return workerEffectiveReferenceIdentity{
			kind: source.Kind(), typeName: source.Type(), pointer: source.Pointer(),
			length: source.Len(), capacity: source.Cap(),
		}, true
	default:
		return workerEffectiveReferenceIdentity{}, false
	}
}
