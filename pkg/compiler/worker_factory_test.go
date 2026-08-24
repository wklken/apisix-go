package compiler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

type workerTestConsumerPreparer struct {
	bindings *runtime.ConsumerBindings
	err      error
}

func (p workerTestConsumerPreparer) PrepareConsumers(
	context.Context,
	PreparationAttempt,
) (*runtime.ConsumerBindings, error) {
	return p.bindings, p.err
}

type workerTestMetadataPreparer struct {
	view runtime.MetadataView
	err  error
}

func (p workerTestMetadataPreparer) PrepareMetadata(
	context.Context,
	PreparationAttempt,
) (runtime.MetadataView, error) {
	return p.view, p.err
}

func TestWorkerCompilerFactoryPrepareGenerationTransfersBaseOwners(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 801, nil, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)

	var trace []string
	materializer.trace = func(stage string) { trace = append(trace, stage) }
	factory.checkpoint = func(stage string, _ workerFactoryCheckpointState) error {
		trace = append(trace, stage)
		return nil
	}
	prepared, err := factory.PrepareGeneration(context.Background(), ticket, desired, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"register-final-publication-set",
		"attempt-and-capability-ready",
		"create-task-registry",
		"prepare-consumers",
		"prepare-metadata",
		"bind-private-materializer-authority",
		"transfer-prepared-generation",
	}
	if !slices.Equal(trace, wantTrace) {
		t.Fatalf("prepare trace = %v, want %v", trace, wantTrace)
	}
	if prepared.attempt.AttemptID() != materializer.registration.id {
		t.Fatal("prepared attempt differs from the registered attempt")
	}
	if prepared.tasks == nil || prepared.consumers == nil || prepared.lookup.bindings == nil ||
		prepared.materializer != materializer || prepared.registry != factory.registry ||
		prepared.cleanup == nil || prepared.manifest != factory.compiler.manifest {
		t.Fatalf("prepared generation did not receive exact base owners: %#v", prepared)
	}
	factory.liveMu.Lock()
	if len(factory.live) != 1 {
		t.Fatalf("live generations = %d, want 1", len(factory.live))
	}
	factory.liveMu.Unlock()

	lookup := prepared.ConsumerLookup()
	if _, ok := lookup.(interface{ Close() }); ok {
		t.Fatalf("production lookup %T exposes Close()", lookup)
	}
	if _, ok := lookup.(interface{ Close(context.Context) error }); ok {
		t.Fatalf("production lookup %T exposes Close(context.Context)", lookup)
	}
	if _, ok := lookup.(*runtime.ConsumerBindings); ok {
		t.Fatalf("production lookup exposes concrete ConsumerBindings")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup.ConsumerByID("missing"); ok {
		t.Fatal("retained lookup remained live after generation close")
	}
	factory.liveMu.Lock()
	defer factory.liveMu.Unlock()
	if len(factory.live) != 0 || materializer.registration.closed != 1 {
		t.Fatalf("close left live=%d registration closes=%d", len(factory.live), materializer.registration.closed)
	}
}

func TestWorkerCompilerFactoryPrepareGenerationConstructsNoPluginsWithoutSpecs(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 802, nil, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := factory.registry.Len(); got != 0 {
		t.Fatalf("resource leases after preparation = %d, want 0", got)
	}
	if prepared.ConsumerLookup() == nil || prepared.PublicationSet().DesiredRevision != desired.Revision() {
		t.Fatal("zero effective specs did not preserve a usable generation owner")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCompilerFactoryPrepareGenerationCleansFailedDependenciesInOrder(t *testing.T) {
	tests := []struct {
		name string
		set  func(*WorkerCompilerFactory, error)
	}{
		{name: "consumer", set: func(factory *WorkerCompilerFactory, injected error) {
			factory.consumers = workerTestConsumerPreparer{err: injected}
		}},
		{name: "metadata", set: func(factory *WorkerCompilerFactory, injected error) {
			factory.metadata = workerTestMetadataPreparer{err: injected}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory, materializer := newWorkerTestFactory(t)
			injected := errors.New("plaintext-primary-must-not-escape")
			var metadataBindings *runtime.ConsumerBindings
			var taskStopped atomic.Bool
			if test.name == "metadata" {
				var bindingsErr error
				metadataBindings, bindingsErr = runtime.NewConsumerBindings([]runtime.ConsumerRecord{{
					ID: "metadata", Consumer: resourceConsumer("metadata"),
				}}, nil, nil)
				if bindingsErr != nil {
					t.Fatal(bindingsErr)
				}
				factory.consumers = workerTestConsumerPreparer{bindings: metadataBindings}
				factory.checkpoint = func(stage string, state workerFactoryCheckpointState) error {
					if stage != "create-task-registry" {
						return nil
					}
					return state.tasks.Go(
						runtime.TaskSpec{Owner: "metadata", Criticality: runtime.TaskCore},
						func(ctx context.Context) error {
							<-ctx.Done()
							taskStopped.Store(true)
							return nil
						},
					)
				}
				materializer.onClose = func() {
					if !taskStopped.Load() {
						t.Error("metadata failure revoked registration before task quiescence")
					}
					if _, ok := metadataBindings.ConsumerByID("metadata"); ok {
						t.Error("metadata failure revoked registration before consumer close")
					}
				}
			}
			test.set(factory, injected)
			desired := mustGenerationSnapshot(t, 803, nil, nil)
			prepared, err := factory.PrepareGeneration(
				context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
			)
			if prepared != nil || err == nil || errors.Is(err, injected) ||
				containsWorkerTestError(err, injected.Error()) {
				t.Fatalf("failure result = %#v/%v, want nil and redacted error", prepared, err)
			}
			if materializer.registration == nil || materializer.registration.closed != 1 {
				t.Fatalf("registration cleanup = %#v", materializer.registration)
			}
			factory.liveMu.Lock()
			live := len(factory.live)
			factory.liveMu.Unlock()
			if live != 0 {
				t.Fatalf("live generations after failure = %d", live)
			}
		})
	}
}

func TestWorkerCompilerFactoryPrepareGenerationPublicationFailureHasNoOwners(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 805, nil, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	ticket.DesiredDigest[0]++
	prepared, err := factory.PrepareGeneration(context.Background(), ticket, desired, nil, nil)
	if prepared != nil || err == nil {
		t.Fatalf("publication failure = %#v/%v", prepared, err)
	}
	if materializer.registration != nil || factory.registry.Len() != 0 {
		t.Fatalf("publication failure created registration/resource = %#v/%d",
			materializer.registration, factory.registry.Len())
	}
	factory.liveMu.Lock()
	defer factory.liveMu.Unlock()
	if len(factory.live) != 0 {
		t.Fatalf("publication failure live owners = %d", len(factory.live))
	}
}

func TestWorkerCompilerFactoryPrepareGenerationCancellationWindows(t *testing.T) {
	stages := []string{
		"attempt-and-capability-ready",
		"create-task-registry",
		"prepare-consumers",
		"prepare-metadata",
		"bind-private-materializer-authority",
	}
	for index, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			factory, materializer := newWorkerTestFactory(t)
			ctx := context.WithValue(context.Background(), workerTestContextKey{}, "retained")
			ctx, cancel := context.WithCancel(ctx)
			factory.checkpoint = func(got string, _ workerFactoryCheckpointState) error {
				if got == stage {
					cancel()
				}
				return nil
			}
			desired := mustGenerationSnapshot(t, uint64(810+index), nil, nil)
			prepared, err := factory.PrepareGeneration(
				ctx, ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
			)
			if prepared != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation result = %#v/%v", prepared, err)
			}
			if materializer.registration == nil || materializer.registration.closed != 1 ||
				materializer.registration.closeCtxErr != nil ||
				materializer.registration.closeValue != "retained" {
				t.Fatalf("cleanup context = %#v", materializer.registration)
			}
			factory.liveMu.Lock()
			live := len(factory.live)
			factory.liveMu.Unlock()
			if live != 0 {
				t.Fatalf("live generations after cancellation = %d", live)
			}
		})
	}
}

func TestWorkerCompilerFactoryPrepareGenerationCallerCancellationDoesNotStopTasks(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	taskContext := make(chan context.Context, 1)
	taskExited := make(chan struct{})
	factory.checkpoint = func(stage string, state workerFactoryCheckpointState) error {
		if stage != "create-task-registry" {
			return nil
		}
		return state.tasks.Go(
			runtime.TaskSpec{Owner: "worker-test", Criticality: runtime.TaskCore},
			func(ctx context.Context) error {
				taskContext <- ctx
				<-ctx.Done()
				close(taskExited)
				return nil
			},
		)
	}
	ctx, cancel := context.WithCancel(context.Background())
	desired := mustGenerationSnapshot(t, 819, nil, nil)
	prepared, err := factory.PrepareGeneration(
		ctx, ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	captured := receiveWorkerTestValue(t, taskContext, "task context")
	cancel()
	if err := captured.Err(); err != nil {
		t.Fatalf("task context after Prepare caller cancel = %v, want nil", err)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	receiveWorkerTestSignal(t, taskExited, "task exit")
	if !errors.Is(captured.Err(), context.Canceled) {
		t.Fatalf("task context after generation close = %v, want context.Canceled", captured.Err())
	}
}

func TestWorkerCompilerFactoryPrepareGenerationRejectsEffectiveConfigCycles(t *testing.T) {
	for _, kind := range []string{"map", "slice", "pointer"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(
				ctx, os.Args[0], "-test.run=^TestWorkerCompilerFactoryPrepareGenerationCycleSubprocess$",
			)
			command.Env = append(os.Environ(), "APISIX_GO_WORKER_FACTORY_CYCLE="+kind)
			command.Stdout = io.Discard
			command.Stderr = io.Discard
			err := command.Run()
			if ctx.Err() != nil {
				t.Fatalf("%s cycle subprocess did not fail closed before timeout", kind)
			}
			if err != nil {
				t.Fatalf("%s cycle subprocess exited with %v, want clean fail-closed rejection", kind, err)
			}
		})
	}
}

func TestWorkerCompilerFactoryPrepareGenerationCycleSubprocess(t *testing.T) {
	kind := os.Getenv("APISIX_GO_WORKER_FACTORY_CYCLE")
	if kind == "" {
		t.Skip("cycle subprocess helper")
	}
	manifest := mustManifest(t)
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	effective := workerTestEffective(manifest)
	switch kind {
	case "map":
		cycle := map[string]any{}
		cycle["self"] = cycle
		effective.Config.PluginAttr = map[string]map[string]any{"cycle": {"value": cycle}}
	case "slice":
		cycle := make([]any, 1)
		cycle[0] = cycle
		effective.Config.PluginAttr = map[string]map[string]any{"cycle": {"value": cycle}}
	case "pointer":
		type cycleNode struct{ Next *cycleNode }
		cycle := &cycleNode{}
		cycle.Next = cycle
		effective.Config.PluginAttr = map[string]map[string]any{"cycle": {"value": cycle}}
	default:
		t.Fatalf("unknown cycle kind %q", kind)
	}
	factory, err := NewWorkerCompilerFactory(
		manifest, effective, &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()},
	)
	if factory != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cycle constructor = %#v/%v, want nil/ErrInvalidInput", factory, err)
	}
}

func TestWorkerCompilerFactoryPrepareGenerationOwnsPartialConsumersBeforeError(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	bindings, err := runtime.NewConsumerBindings([]runtime.ConsumerRecord{{
		ID: "partial", Consumer: resourceConsumer("partial"),
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	injected := &workerTestSecretError{text: "secret-consumer-error"}
	factory.consumers = workerTestConsumerPreparer{bindings: bindings, err: injected}
	var taskStopped atomic.Bool
	factory.checkpoint = func(stage string, state workerFactoryCheckpointState) error {
		if stage != "create-task-registry" {
			return nil
		}
		return state.tasks.Go(
			runtime.TaskSpec{Owner: "partial", Criticality: runtime.TaskCore},
			func(ctx context.Context) error {
				<-ctx.Done()
				taskStopped.Store(true)
				return nil
			},
		)
	}
	materializer.onClose = func() {
		if !taskStopped.Load() {
			t.Error("registration revoked before task quiescence")
		}
		if _, ok := bindings.ConsumerByID("partial"); ok {
			t.Error("registration revoked before partial consumer close")
		}
	}
	desired := mustGenerationSnapshot(t, 821, nil, nil)
	prepared, prepareErr := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if prepared != nil || prepareErr == nil {
		t.Fatalf("partial consumer result = %#v/%v", prepared, prepareErr)
	}
	assertWorkerErrorRedacted(t, prepareErr, injected)

	factory, _ = newWorkerTestFactory(t)
	factory.consumers = workerTestConsumerPreparer{}
	desired = mustGenerationSnapshot(t, 822, nil, nil)
	if prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	); prepared != nil || err == nil {
		t.Fatalf("nil consumer result = %#v/%v, want failure", prepared, err)
	}
}

func TestWorkerCompilerFactoryPrepareGenerationRedactsProviderAndCleanupErrors(t *testing.T) {
	t.Run("registration primary and partial cleanup", func(t *testing.T) {
		factory, materializer := newWorkerTestFactory(t)
		primary := &workerTestSecretError{text: "secret-registration-primary"}
		cleanup := &workerTestSecretError{text: "secret-registration-cleanup"}
		materializer.registerErr = primary
		materializer.closeErr = cleanup
		desired := mustGenerationSnapshot(t, 826, nil, nil)
		_, err := factory.PrepareGeneration(
			context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
		)
		assertWorkerErrorRedacted(t, err, primary, cleanup)
	})
	t.Run("consumer primary and registration cleanup", func(t *testing.T) {
		factory, materializer := newWorkerTestFactory(t)
		primary := &workerTestSecretError{text: "secret-primary"}
		cleanup := &workerTestSecretError{text: "secret-cleanup"}
		materializer.closeErr = cleanup
		factory.consumers = workerTestConsumerPreparer{err: primary}
		desired := mustGenerationSnapshot(t, 823, nil, nil)
		_, err := factory.PrepareGeneration(
			context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
		)
		assertWorkerErrorRedacted(t, err, primary, cleanup)
	})
	t.Run("wrapped context source", func(t *testing.T) {
		factory, _ := newWorkerTestFactory(t)
		secretCause := &workerTestSecretError{text: "secret-context-wrapper"}
		factory.metadata = workerTestMetadataPreparer{err: errors.Join(context.Canceled, secretCause)}
		desired := mustGenerationSnapshot(t, 824, nil, nil)
		_, err := factory.PrepareGeneration(
			context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want exact reconstructed cancellation", err)
		}
		assertWorkerErrorRedacted(t, err, secretCause)
	})
	t.Run("successful transfer registration close", func(t *testing.T) {
		factory, materializer := newWorkerTestFactory(t)
		cleanup := &workerTestSecretError{text: "secret-close-after-transfer"}
		materializer.closeErr = cleanup
		desired := mustGenerationSnapshot(t, 825, nil, nil)
		prepared, err := factory.PrepareGeneration(
			context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		assertWorkerErrorRedacted(t, prepared.Close(context.Background()), cleanup)
	})
}

func TestWorkerCompilerFactoryPrepareGenerationOwnsEffectiveConfig(t *testing.T) {
	manifest := mustManifest(t)
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	effective := workerTestEffective(manifest)
	shared := map[string]any{"value": []string{"shared"}}
	effective.Config.PluginAttr = map[string]map[string]any{
		"example": {
			"nested":   map[string]any{"slice": []any{"original"}, "typed": []string{"typed"}},
			"shared-a": shared, "shared-b": shared,
		},
	}
	effective.Provenance = config.Provenance{
		"plugin_attr.example": {Kind: config.SourceCLI, Origin: "original", Explicit: true},
	}
	factory, err := NewWorkerCompilerFactory(
		manifest, effective, &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()},
	)
	if err != nil {
		t.Fatal(err)
	}
	nested := effective.Config.PluginAttr["example"]["nested"].(map[string]any)
	nested["slice"].([]any)[0] = "mutated"
	nested["typed"].([]string)[0] = "mutated"
	provenance := effective.Provenance["plugin_attr.example"]
	provenance.Origin = "mutated"
	effective.Provenance["plugin_attr.example"] = provenance
	ownedNested := factory.effective.Config.PluginAttr["example"]["nested"].(map[string]any)
	if ownedNested["slice"].([]any)[0] != "original" || ownedNested["typed"].([]string)[0] != "typed" ||
		factory.effective.Provenance["plugin_attr.example"].Origin != "original" {
		t.Fatalf("owned effective config aliased caller: %#v", factory.effective)
	}
	if factory.effective.Config.PluginAttr["example"]["shared-a"].(map[string]any)["value"].([]string)[0] !=
		"shared" {
		t.Fatal("acyclic shared alias was rejected or changed concrete type")
	}

	mismatch := workerTestEffective(manifest)
	mismatch.Profiles.Security = config.SecurityStrict
	if got, err := NewWorkerCompilerFactory(
		manifest, mismatch, &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()},
	); got != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("profile mismatch = %#v/%v", got, err)
	}
	invalid := workerTestEffective(manifest)
	invalid.Config.SecurityProfile = config.SecurityProfile("invalid")
	invalid.Profiles = invalid.Config.Profiles()
	if got, err := NewWorkerCompilerFactory(
		manifest, invalid, &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()},
	); got != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid profile = %#v/%v", got, err)
	}
	opaque := workerTestEffective(manifest)
	opaque.Config.PluginAttr = map[string]map[string]any{"bad": {"callback": func() {}}}
	if got, err := NewWorkerCompilerFactory(
		manifest, opaque, &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()},
	); got != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("opaque mutable effective value = %#v/%v", got, err)
	}
}

func TestWorkerCompilerFactoryPrepareGenerationConstructorValidation(t *testing.T) {
	manifest := mustManifest(t)
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	valid := &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()}
	var typedNil *workerTestMaterializer
	tests := []struct {
		name         string
		manifest     *capability.Manifest
		effective    *config.EffectiveConfig
		materializer secret.Materializer
	}{
		{name: "nil manifest", effective: workerTestEffective(manifest), materializer: valid},
		{name: "nil effective", manifest: manifest, materializer: valid},
		{name: "nil materializer", manifest: manifest, effective: workerTestEffective(manifest)},
		{
			name: "typed nil materializer", manifest: manifest,
			effective: workerTestEffective(manifest), materializer: typedNil,
		},
		{
			name: "digest mismatch", manifest: manifest,
			effective: workerTestEffective(manifest), materializer: &workerTestMaterializer{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory, err := NewWorkerCompilerFactory(test.manifest, test.effective, test.materializer)
			if factory != nil || !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("constructor = %#v/%v, want nil/ErrInvalidInput", factory, err)
			}
		})
	}
}

func TestWorkerCompilerFactoryPrepareGenerationUsesRealAliasAndCatalog(t *testing.T) {
	manifest := mustManifest(t)
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	materializer := &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()}
	factory, err := NewWorkerCompilerFactory(manifest, workerTestEffective(manifest), materializer)
	if err != nil {
		t.Fatal(err)
	}
	_, hasOTel := factory.compiler.manifest.Plugin("otel")
	_, hasOpenTelemetry := factory.compiler.manifest.Plugin("opentelemetry")
	if !hasOTel || !hasOpenTelemetry {
		t.Fatal("validated factory manifest lost the accepted otel alias")
	}

	desired := mustGenerationSnapshot(t, 820, []generation.Resource{
		resourceValue("plugin_metadata", "otel", `{"trace_id_source":"random"}`),
	}, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if ok, decodeErr := prepared.MetadataView().Decode("otel", &metadata); decodeErr != nil || !ok {
		t.Fatalf("otel metadata = %#v/%v/%v", metadata, ok, decodeErr)
	}
	if ok, decodeErr := prepared.MetadataView().Decode("opentelemetry", &metadata); decodeErr != nil || ok {
		t.Fatalf("opentelemetry metadata unexpectedly produced = %v/%v", ok, decodeErr)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	declarations := factory.compiler.schemas.catalog.Declarations()
	wants := []capability.SecretDeclaration{
		{Factory: "azure-functions", Source: capability.SecretPluginConfig, Field: "authorization.apikey"},
		{Factory: "error-log-logger", Source: capability.SecretPluginMetadata, Field: "clickhouse.password"},
		{
			Factory: "error-log-logger", Source: capability.SecretPluginMetadata,
			Field: "kafka.brokers.*.sasl_config.password",
		},
	}
	for _, want := range wants {
		if !slices.ContainsFunc(declarations, func(got capability.SecretDeclaration) bool {
			return got.Factory == want.Factory && got.Source == want.Source && got.Field == want.Field
		}) {
			t.Fatalf("real declaration catalog lacks %+v", want)
		}
	}
}

func TestWorkerCompilerFactoryPrepareGenerationOwnsInputsAndOutputs(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 830, []generation.Resource{
		resourceValue("routes", "owned", `{"id":"owned","plugins":{"request-id":{}}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	previousSnapshot := mustGenerationSnapshot(t, 829, []generation.Resource{
		resourceValue("routes", "owned", `{"id":"owned","plugins":{"request-id":{}}}`),
	}, nil)
	previous := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, previousSnapshot),
	}
	prepared, err := factory.PrepareGeneration(context.Background(), ticket, desired, previous, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := prepared.PublicationSet()
	ticket.RequiredDomains[0] = generation.DomainStream
	previous[generation.DomainHTTP] = generation.PublishedGeneration{}
	resources := desired.Resources()
	resources[0].Value[0] = 'x'
	returned := prepared.PublicationSet()
	delete(returned.Domains, generation.DomainHTTP)
	if got := prepared.PublicationSet(); !reflect.DeepEqual(got, want) ||
		!reflect.DeepEqual(materializer.registered, want) {
		t.Fatalf("caller mutation changed prepared=%#v registered=%#v want=%#v", got, materializer.registered, want)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCompilerFactoryPrepareGenerationConcurrentOwners(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	desiredA := mustGenerationSnapshot(t, 840, nil, nil)
	desiredB := mustGenerationSnapshot(t, 841, nil, nil)
	results := make(chan *PreparedGeneration, 2)
	errorsOut := make(chan error, 2)
	var wait sync.WaitGroup
	for _, desired := range []generation.Snapshot{desiredA, desiredB} {
		wait.Go(func() {
			prepared, err := factory.PrepareGeneration(
				context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
			)
			results <- prepared
			errorsOut <- err
		})
	}
	wait.Wait()
	close(results)
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	var prepared []*PreparedGeneration
	for result := range results {
		prepared = append(prepared, result)
	}
	if len(prepared) != 2 || prepared[0].tasks == prepared[1].tasks ||
		prepared[0].attempt.AttemptID() == prepared[1].attempt.AttemptID() ||
		prepared[0].cleanup == prepared[1].cleanup || prepared[0].consumers == prepared[1].consumers ||
		prepared[0].registry != prepared[1].registry || prepared[0].registry != factory.registry {
		t.Fatalf("concurrent owner identities are not isolated: %#v", prepared)
	}
	for _, generationOwner := range prepared {
		if err := generationOwner.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerCompilerFactoryPrepareGenerationRejectsLiveAttemptCollision(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 850, nil, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	start := make(chan struct{})
	results := make(chan *PreparedGeneration, 2)
	errorsOut := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			<-start
			prepared, err := factory.PrepareGeneration(context.Background(), ticket, desired, nil, nil)
			results <- prepared
			errorsOut <- err
		})
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsOut)
	var successful *PreparedGeneration
	successes := 0
	failures := 0
	for prepared := range results {
		if prepared != nil {
			successes++
			successful = prepared
		}
	}
	for err := range errorsOut {
		if err != nil {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("collision successes/failures = %d/%d, want 1/1", successes, failures)
	}
	factory.liveMu.Lock()
	tracked := factory.live[successful.attempt.AttemptID()]
	factory.liveMu.Unlock()
	if tracked != successful {
		t.Fatal("collision replaced the previously tracked live owner")
	}
	if len(materializer.registrations) != 2 {
		t.Fatalf("registrations = %d, want 2", len(materializer.registrations))
	}
	closedBeforeOwner := 0
	for _, registration := range materializer.registrations {
		closedBeforeOwner += registration.closed
	}
	if closedBeforeOwner != 1 {
		t.Fatalf("registration closes before owner close = %d, want failed candidate only", closedBeforeOwner)
	}
	if err := successful.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	retry, err := factory.PrepareGeneration(context.Background(), ticket, desired, nil, nil)
	if err != nil {
		t.Fatalf("sequential retry after detach failed: %v", err)
	}
	if err := retry.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type workerTestContextKey struct{}

type workerTestRegistration struct {
	id          secret.AttemptID
	closeCtxErr error
	closeValue  any
	closed      int
	closeErr    error
	onClose     func()
}

func (r *workerTestRegistration) AttemptID() secret.AttemptID { return r.id }

func (*workerTestRegistration) Materialize(
	context.Context,
	secret.Scope,
	string,
) (secret.Value, error) {
	return secret.Value{}, errors.New("unexpected materialization")
}

func (r *workerTestRegistration) Close(ctx context.Context) error {
	r.closed++
	r.closeCtxErr = ctx.Err()
	r.closeValue = ctx.Value(workerTestContextKey{})
	if r.onClose != nil {
		r.onClose()
	}
	return r.closeErr
}

type workerTestMaterializer struct {
	mu            sync.Mutex
	digest        [32]byte
	registration  *workerTestRegistration
	registrations []*workerTestRegistration
	registered    generation.PublicationSet
	trace         func(string)
	closeErr      error
	onClose       func()
	registerErr   error
}

func (m *workerTestMaterializer) RegisterCandidate(
	_ context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registered = clonePublicationSetForPreparation(set)
	if m.trace != nil {
		m.trace("register-final-publication-set")
	}
	m.registration = &workerTestRegistration{
		id: secret.CandidateAttemptID(ticket, set), closeErr: m.closeErr, onClose: m.onClose,
	}
	m.registrations = append(m.registrations, m.registration)
	return m.registration, m.registerErr
}

func (*workerTestMaterializer) RegisterRecovery(
	context.Context,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) (secret.AttemptRegistration, error) {
	return nil, errors.New("unexpected recovery registration")
}

func (m *workerTestMaterializer) DeclarationDigest() [32]byte {
	if m == nil {
		return [32]byte{}
	}
	return m.digest
}

func newWorkerTestFactory(t *testing.T) (*WorkerCompilerFactory, *workerTestMaterializer) {
	t.Helper()
	manifest := mustManifest(t)
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	materializer := &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()}
	factory, err := NewWorkerCompilerFactory(manifest, workerTestEffective(manifest), materializer)
	if err != nil {
		t.Fatal(err)
	}
	return factory, materializer
}

func containsWorkerTestError(err error, text string) bool {
	return err != nil && strings.Contains(err.Error(), text)
}

func workerTestEffective(manifest *capability.Manifest) *config.EffectiveConfig {
	profiles := config.ProfileSelection{
		Compatibility: config.CompatibilityTarget(manifest.Target.Name),
		Security:      config.SecurityCompat,
	}
	return &config.EffectiveConfig{
		Config: config.Config{
			CompatibilityTarget:  profiles.Compatibility,
			SecurityProfile:      profiles.Security,
			QualificationProfile: profiles.Qualification,
		},
		Profiles: profiles,
	}
}

func resourceConsumer(id string) resource.Consumer {
	return resource.Consumer{Username: id}
}

type workerTestSecretError struct{ text string }

func (err *workerTestSecretError) Error() string { return err.text }

func assertWorkerErrorRedacted(t *testing.T, err error, secrets ...error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want redacted failure")
	}
	formatted := []string{err.Error(), fmt.Sprint(err), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)}
	for _, secretErr := range secrets {
		if secretErr == nil {
			continue
		}
		for _, output := range formatted {
			if strings.Contains(output, secretErr.Error()) {
				t.Fatalf("redacted error exposes %q in %q", secretErr.Error(), output)
			}
		}
		if errors.Is(err, secretErr) {
			t.Fatalf("redacted error retains secret cause through errors.Is: %v", err)
		}
		var target *workerTestSecretError
		if errors.As(err, &target) {
			t.Fatalf("redacted error retains secret cause through errors.As: %v", target)
		}
	}
	assertWorkerErrorTreeSafe(t, err, secrets)
}

func assertWorkerErrorTreeSafe(t *testing.T, err error, secrets []error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secretErr := range secrets {
		if secretErr != nil && strings.Contains(err.Error(), secretErr.Error()) {
			t.Fatalf("error tree node %T exposes secret: %v", err, err)
		}
	}
	switch value := err.(type) {
	case interface{ Unwrap() []error }:
		for _, nested := range value.Unwrap() {
			assertWorkerErrorTreeSafe(t, nested, secrets)
		}
	case interface{ Unwrap() error }:
		assertWorkerErrorTreeSafe(t, value.Unwrap(), secrets)
	}
}

func receiveWorkerTestValue[T any](t *testing.T, values <-chan T, name string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		var zero T
		return zero
	}
}

func receiveWorkerTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
