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
	bindingOps         effectiveBindingOps
	trustedClientCAPEM []byte

	gate   sync.RWMutex
	closed bool

	liveMu sync.Mutex
	live   map[secret.AttemptID]*PreparedGeneration

	closeOnce sync.Once
	closeErr  error

	checkpoint func(string, workerFactoryCheckpointState) error
}

// NewWorkerCompilerFactory constructs the candidate-generation compiler and
// validates every immutable authority before allocating lifecycle owners.
func NewWorkerCompilerFactory(
	manifest *capability.Manifest,
	effective *config.EffectiveConfig,
	materializer secret.Materializer,
) (*WorkerCompilerFactory, error) {
	if manifest == nil || effective == nil || isNilInterface(materializer) {
		return nil, fmt.Errorf("%w: worker compiler dependencies are required", ErrInvalidInput)
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
		registry: runtime.NewResourceRegistry(), bindingOps: defaultEffectiveBindingOps(),
		trustedClientCAPEM: trustedClientCAPEM,
		live:               make(map[secret.AttemptID]*PreparedGeneration),
	}, nil
}

func readWorkerTrustedClientCA(cfg *config.Config) ([]byte, error) {
	if cfg == nil {
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
// resource registry. Repeated calls replay the first safe cleanup result.
func (factory *WorkerCompilerFactory) Close(ctx context.Context) error {
	if factory == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx := context.WithoutCancel(ctx)
	factory.closeOnce.Do(func() {
		factory.gate.Lock()
		factory.closed = true
		factory.liveMu.Lock()
		type liveGeneration struct {
			attemptID secret.AttemptID
			prepared  *PreparedGeneration
		}
		live := make([]liveGeneration, 0, len(factory.live))
		for attemptID, prepared := range factory.live {
			live = append(live, liveGeneration{attemptID: attemptID, prepared: prepared})
		}
		factory.liveMu.Unlock()
		factory.gate.Unlock()

		slices.SortFunc(live, func(left, right liveGeneration) int {
			return bytes.Compare(left.attemptID[:], right.attemptID[:])
		})
		cleanupFailed := false
		for _, generationOwner := range live {
			if generationOwner.prepared.Close(cleanupCtx) != nil {
				cleanupFailed = true
			}
		}
		if factory.registry != nil && factory.registry.Close(cleanupCtx) != nil {
			cleanupFailed = true
		}
		if cleanupFailed {
			factory.closeErr = errWorkerCompilerFactoryCleanupFailed
		}
	})
	return factory.closeErr
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
			return errWorkerGenerationCleanupFailed
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
